// @author: kevinkelly
// @license: Apache Licence
// @file: harness.go
// @time: 2026/10/28

package enginetest

import (
	"fmt"
	"time"

	"dbtool/internal/archive/engine"
)

// Harness 把全部替身装配在一起：
//
//	h := enginetest.New(enginetest.Options{Rows: 2500})
//	res, err := engine.New().Run(ctx, h.Spec(), h.Policy(), h.Env(), ctl, h.Sink)
//
// 默认：库 shop，表 orders（id BIGINT 主键，created_at，status，payload），归档库表 shop_arch.orders_bak（同实例），
// 假时钟从 2026-10-08 02:00 开始，窗口不限制。
type Harness struct {
	Clock *Clock
	Log   *OpLog
	Src   *MemSource
	Dst   *MemDest
	Files *MemStore
	Probe *MemProbe
	Sink  *Collector

	FileKey []byte
	Opts    Options
}

// Options 控制 New 生成的数据。
type Options struct {
	Rows       int       // 源表行数，默认 1000
	CompoundPK bool      // true 时主键是 (tenant, id)，tenant 为 "a" / "b" 交替分段
	Start      time.Time // 假时钟起点
}

// DefaultColumns 是 shop.orders 的列。
func DefaultColumns() []engine.ColumnInfo {
	return []engine.ColumnInfo{
		{Name: "id", Type: "bigint(20)"}, {Name: "created_at", Type: "datetime"},
		{Name: "status", Type: "int(11)", Nullable: true}, {Name: "payload", Type: "varchar(64)", Nullable: true},
	}
}

// New 创建 Harness 并往源表里灌 Rows 行（id 从 1 开始）。
func New(o Options) *Harness {
	if o.Rows == 0 {
		o.Rows = 1000
	}
	if o.Start.IsZero() {
		o.Start = time.Date(2026, 10, 8, 2, 0, 0, 0, time.Local)
	}
	h := &Harness{Opts: o, Clock: NewClock(o.Start), Log: &OpLog{}, Sink: &Collector{}, Probe: &MemProbe{}}
	h.FileKey = make([]byte, 32)
	for i := range h.FileKey {
		h.FileKey[i] = byte(i + 1)
	}
	cols := DefaultColumns()
	pk := []string{"id"}
	if o.CompoundPK {
		cols = append([]engine.ColumnInfo{{Name: "tenant", Type: "varchar(8)"}}, cols...)
		pk = []string{"tenant", "id"}
	}
	h.Src = NewMemSource(h.Log, h.Clock, "shop", "orders", cols, pk)
	var seed []engine.Row
	for i := 1; i <= o.Rows; i++ {
		row := engine.Row{int64(i), fmt.Sprintf("2025-01-%02d 10:00:00", i%28+1), int64(i % 5), fmt.Sprintf("p%d", i)}
		if o.CompoundPK {
			tenant := "a"
			if i > o.Rows/2 {
				tenant = "b"
			}
			row = append(engine.Row{tenant}, row...)
		}
		seed = append(seed, row)
	}
	h.Src.Seed(seed...)
	h.Dst = NewMemDest(h.Log, h.Clock, "shop_arch")
	h.Files = NewMemStore(h.Log)
	return h
}

// Spec 返回默认任务规格：归档并删除，备份到库表 shop_arch.orders_bak，保留 90 天，批 100，休眠 50 ms。
func (h *Harness) Spec() engine.Spec {
	pk := h.Src.Info.PrimaryKey
	return engine.Spec{
		TaskID: 1, RunID: 1, Name: "orders 归档",
		InstanceID: 1, DB: "shop", Table: "orders",
		TimeColumn: "created_at", RetentionDays: 90,
		Mode:        engine.ModeMove,
		Dest:        &engine.TableDest{InstanceID: 1, DB: "shop_arch", Table: "orders_bak"},
		Params:      engine.Params{BatchSize: 100, SleepMs: 50},
		Fingerprint: engine.Fingerprint(h.Src.Info.Columns, pk),
	}
}

// FileSpec 在默认规格上改成“只写文件”备份。
func (h *Harness) FileSpec(format string, gz, enc bool) engine.Spec {
	s := h.Spec()
	s.Dest = nil
	s.File = &engine.FileDest{StoreID: 1, Format: format, Gzip: gz, Encrypt: enc, PathTemplate: "{inst}/{db}/{table}/{yyyy}/{mm}/{dd}/{run_id}"}
	return s
}

// Policy 返回默认安全策略。
func (h *Harness) Policy() engine.Policy {
	return engine.Policy{MinRetentionDays: 30, ProtectedPatterns: []string{"mysql", "sys"}, MaxRowsPerRun: 5_000_000}
}

// Env 返回装配好的 engine.Env：总是带 Src 和 Probe；Dst / Files 由规格决定（用 EnvFor 更方便）。
func (h *Harness) Env() *engine.Env {
	return &engine.Env{Src: h.Src, Dst: h.Dst, Files: h.Files, Probe: h.Probe, FileKey: h.FileKey,
		Now: h.Clock.Now, Sleep: h.Clock.Sleep, InstName: "prod-db01", TempDir: ""}
}

// EnvFor 只装配规格里用到的去向（没有库表备份时 Dst=nil，没有文件备份时 Files=nil）。
func (h *Harness) EnvFor(spec engine.Spec, tempDir string) *engine.Env {
	e := h.Env()
	e.TempDir = tempDir
	if spec.Dest == nil {
		e.Dst = nil
	}
	if spec.File == nil {
		e.Files = nil
	}
	return e
}
