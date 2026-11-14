// @author: kevinkelly
// @license: Apache Licence
// @file: source.go
// @time: 2026/10/28

package enginetest

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dbtool/internal/archive/engine"
)

// pkStr 把值转成主键的字符串形式（与真实实现一致：数值十进制、[]byte 按字符串）。
func pkStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(x)
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	}
	return fmt.Sprint(v)
}

func isNumericType(t string) bool {
	t = strings.ToLower(t)
	for _, p := range []string{"tinyint", "smallint", "mediumint", "int", "bigint", "decimal"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// ComparePK 按主键列顺序比较两个主键值（数值列按数值，其余按字典序）。
func ComparePK(numeric []bool, a, b []string) int {
	for i := range a {
		if i >= len(b) {
			return 1
		}
		var c int
		if i < len(numeric) && numeric[i] {
			x, _ := strconv.ParseInt(a[i], 10, 64)
			y, _ := strconv.ParseInt(b[i], 10, 64)
			switch {
			case x < y:
				c = -1
			case x > y:
				c = 1
			}
		} else {
			c = strings.Compare(a[i], b[i])
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

// MemSource 是内存里的源表（只有一张表）。ReadBatch 返回主键大于断点的行（忽略 Where：表里的行就是“符合条件的行”）。
type MemSource struct {
	mu    sync.Mutex
	Log   *OpLog
	Clock *Clock
	DB    string
	Table string

	Info    engine.TableInfo
	rows    []engine.Row
	pkIdx   []int
	numeric []bool

	// 钩子（call 从 1 开始计数）。返回错误即模拟该次调用失败。
	OnRead         func(call int, req engine.ReadReq) error
	OnDelete       func(call int, req engine.DeleteReq) error
	OnInsertIgnore func(call int, req engine.InsertReq) error
	// ReadCost / DeleteCost 返回该次调用“花费”的时间，会推进 Clock（用来测批耗时自适应）。
	ReadCost   func(call int) time.Duration
	DeleteCost func(call int) time.Duration
	// DeleteAffected 可改写 DeleteBatch 返回的受影响行数（actual 是实际删除的行数）。
	DeleteAffected func(call int, actual int64) int64

	// 试运行用
	RowsOverride int64 // >0 时 Describe 返回的 Rows 用它（模拟 information_schema 的行数估算）
	ExplainRes   engine.ExplainResult
	ExplainErr   error
	CountN       int64 // <0 表示用当前行数
	CountCapped  bool
	CountErr     error
	ProbeRes     engine.TimeProbe // 整型时间戳列的探测结果（engine.TimeProber）
	ProbeErr     error
	ProbeCalls   int
	ProbeRangeOK bool // 最近一次探测传入的 rangeOK

	// 记录
	Reads, Deletes, Inserts int
	ReadReqs                []engine.ReadReq
	DeleteReqs              []engine.DeleteReq
	Wheres                  []string
}

// NewMemSource 创建源表。cols 的 Type 决定主键是否按数值比较。
func NewMemSource(log *OpLog, clock *Clock, db, table string, cols []engine.ColumnInfo, pk []string) *MemSource {
	s := &MemSource{Log: log, Clock: clock, DB: db, Table: table, CountN: -1}
	for _, p := range pk {
		for i, c := range cols {
			if strings.EqualFold(c.Name, p) {
				s.pkIdx = append(s.pkIdx, i)
				s.numeric = append(s.numeric, isNumericType(c.Type))
			}
		}
	}
	idx := map[string][]string{}
	if len(pk) > 0 {
		idx["PRIMARY"] = pk
	}
	s.Info = engine.TableInfo{DB: db, Name: table, Exists: true, Engine: "InnoDB", Columns: cols, PrimaryKey: pk, Indexes: idx,
		CreateSQL: "CREATE TABLE `" + table + "` (...)", Fingerprint: engine.Fingerprint(cols, pk)}
	return s
}

func (s *MemSource) pkOf(r engine.Row) []string {
	out := make([]string, len(s.pkIdx))
	for i, ix := range s.pkIdx {
		out[i] = pkStr(r[ix])
	}
	return out
}

// Seed 追加行并按主键排序。
func (s *MemSource) Seed(rows ...engine.Row) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, rows...)
	sort.SliceStable(s.rows, func(i, j int) bool { return ComparePK(s.numeric, s.pkOf(s.rows[i]), s.pkOf(s.rows[j])) < 0 })
	s.Info.Rows = int64(len(s.rows))
}

// Len 返回当前行数。
func (s *MemSource) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows)
}

// Rows 返回当前所有行的副本。
func (s *MemSource) Rows() []engine.Row {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]engine.Row, len(s.rows))
	for i, r := range s.rows {
		out[i] = append(engine.Row(nil), r...)
	}
	return out
}

// Has 判断某主键（字符串形式，复合主键用逗号连接）是否还在表里。
func (s *MemSource) Has(pk string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rows {
		if strings.Join(s.pkOf(r), ",") == pk {
			return true
		}
	}
	return false
}

// SetFingerprint 模拟表结构变化。
func (s *MemSource) SetFingerprint(fp string) {
	s.mu.Lock()
	s.Info.Fingerprint = fp
	s.mu.Unlock()
}

func (s *MemSource) spend(d time.Duration) {
	if d > 0 && s.Clock != nil {
		s.Clock.Advance(d)
	}
}

func (s *MemSource) Describe(ctx context.Context, db, table string) (engine.TableInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if db != s.DB || table != s.Table {
		return engine.TableInfo{DB: db, Name: table}, nil
	}
	info := s.Info
	info.Rows = int64(len(s.rows))
	if s.RowsOverride > 0 {
		info.Rows = s.RowsOverride
	}
	return info, nil
}

func (s *MemSource) Fingerprint(ctx context.Context, db, table string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Info.Fingerprint, nil
}

func (s *MemSource) Explain(ctx context.Context, query string, args ...any) (engine.ExplainResult, error) {
	return s.ExplainRes, s.ExplainErr
}

func (s *MemSource) CountWhere(ctx context.Context, db, table, where string, limit int64) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.CountErr != nil {
		return 0, false, s.CountErr
	}
	n := s.CountN
	if n < 0 {
		n = int64(len(s.rows))
	}
	capped := s.CountCapped
	if n > limit {
		n, capped = limit, true
	}
	return n, capped, nil
}

func (s *MemSource) Sample(ctx context.Context, db, table, where string, cols []string, limit int) ([]map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]string
	for i, r := range s.rows {
		if i >= limit {
			break
		}
		m := map[string]string{}
		for j, c := range s.Info.Columns {
			if j < len(r) {
				m[c.Name] = fmt.Sprint(r[j])
			}
		}
		out = append(out, m)
	}
	return out, nil
}

func (s *MemSource) ReadBatch(ctx context.Context, req engine.ReadReq) (engine.Batch, error) {
	s.mu.Lock()
	s.Reads++
	call := s.Reads
	s.ReadReqs = append(s.ReadReqs, req)
	s.Wheres = append(s.Wheres, req.Where)
	hook, cost := s.OnRead, s.ReadCost
	s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return engine.Batch{}, err
	}
	if hook != nil {
		if err := hook(call, req); err != nil {
			return engine.Batch{}, err
		}
	}
	if cost != nil {
		s.spend(cost(call))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := engine.Batch{}
	for _, c := range s.Info.Columns {
		b.Cols = append(b.Cols, c.Name)
	}
	start := 0
	if len(req.After) > 0 && !req.Unordered { // 行按主键有序，二分找第一个 > 断点的行；不排序读取没有断点，取当前剩下的前 Limit 行
		start = sort.Search(len(s.rows), func(i int) bool { return ComparePK(s.numeric, s.pkOf(s.rows[i]), req.After) > 0 })
	}
	for _, r := range s.rows[start:] {
		pk := s.pkOf(r)
		b.Rows = append(b.Rows, append(engine.Row(nil), r...))
		b.PKs = append(b.PKs, pk)
		if len(b.Rows) >= req.Limit {
			break
		}
	}
	s.Log.add("src.read", b.PKs, len(b.Rows))
	return b, nil
}

func (s *MemSource) DeleteBatch(ctx context.Context, req engine.DeleteReq) (int64, error) {
	s.mu.Lock()
	s.Deletes++
	call := s.Deletes
	s.DeleteReqs = append(s.DeleteReqs, req)
	hook, cost, adj := s.OnDelete, s.DeleteCost, s.DeleteAffected
	s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if hook != nil {
		if err := hook(call, req); err != nil {
			return 0, err
		}
	}
	if cost != nil {
		s.spend(cost(call))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	del := map[string]bool{}
	for _, pk := range req.PKs {
		del[strings.Join(pk, ",")] = true
	}
	kept := s.rows[:0:0]
	var n int64
	for _, r := range s.rows {
		if del[strings.Join(s.pkOf(r), ",")] {
			n++
			continue
		}
		kept = append(kept, r)
	}
	s.rows = kept
	s.Log.add("src.delete", req.PKs, int(n))
	if adj != nil {
		n = adj(call, n)
	}
	return n, nil
}

func (s *MemSource) InsertIgnore(ctx context.Context, req engine.InsertReq) (int64, error) {
	s.mu.Lock()
	s.Inserts++
	call := s.Inserts
	hook := s.OnInsertIgnore
	s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if hook != nil {
		if err := hook(call, req); err != nil {
			return 0, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	have := map[string]bool{}
	for _, r := range s.rows {
		have[strings.Join(s.pkOf(r), ",")] = true
	}
	var n int64
	var pks [][]string
	for _, r := range req.Rows {
		k := strings.Join(s.pkOf(r), ",")
		if have[k] {
			continue
		}
		have[k] = true
		s.rows = append(s.rows, append(engine.Row(nil), r...))
		pks = append(pks, s.pkOf(r))
		n++
	}
	sort.SliceStable(s.rows, func(i, j int) bool { return ComparePK(s.numeric, s.pkOf(s.rows[i]), s.pkOf(s.rows[j])) < 0 })
	s.Log.add("src.insert_ignore", pks, int(n))
	return n, nil
}

// ProbeTime 实现 engine.TimeProber：返回预设的 ProbeRes / ProbeErr。
func (s *MemSource) ProbeTime(ctx context.Context, db, table, col string, rangeOK bool) (engine.TimeProbe, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ProbeCalls++
	s.ProbeRangeOK = rangeOK
	return s.ProbeRes, s.ProbeErr
}

func (s *MemSource) Close() error { return nil }

var (
	_ engine.SourceDB   = (*MemSource)(nil)
	_ engine.TimeProber = (*MemSource)(nil)
)
