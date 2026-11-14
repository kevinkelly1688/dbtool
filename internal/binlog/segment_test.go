// @author: kevinkelly
// @license: Apache Licence
// @file: segment_test.go
// @time: 2026/10/28

package binlog

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-mysql-org/go-mysql/replication"
)

// stallStream 发完给定的事件后报告 errStalled（一段时间没有新事件，服务端也没发“没有更多事件”的 EOF 包）。
type stallStream struct{ evs []*replication.BinlogEvent }

func (s *stallStream) Next(ctx context.Context) (*replication.BinlogEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(s.evs) == 0 {
		return nil, errStalled
	}
	ev := s.evs[0]
	s.evs = s.evs[1:]
	return ev, nil
}
func (s *stallStream) Close() error { return nil }

// scenario() 的事件顺序（含开头的 ROTATE）：
// 1 rotate 2 fde 3 begin 4 tm 5 insert 6 tm 7 update 8 xid | 9 begin 10 tm 11 delete 12 xid
const (
	evAfterInsert  = 5  // 事务 1 中间
	evAfterTrx1    = 8  // 事务 1 刚提交
	evAfterDeleteA = 11 // 事务 2 中间
)

// scanN 只发前 n 个事件然后“卡住”（n=0 表示全部发完再卡住）。spec 里带上开始时最后一个文件的大小，用来校验有没有读到末尾。
func scanN(t *testing.T, b *binBuilder, spec scanSpec, n int) (*scanResult, error, []*replication.BinlogEvent) {
	t.Helper()
	evs := append([]*replication.BinlogEvent{rotateEvent(t, "bin.000001")}, b.events(t)...)
	send := evs
	if n > 0 {
		send = evs[:n]
	}
	spec.LastFile, spec.LastSize = "bin.000001", int64(b.pos)
	spec.StartPos = 4
	res, err := scan(context.Background(), spec, &stallStream{evs: append([]*replication.BinlogEvent(nil), send...)}, newFakeMeta(), &progress{})
	if res != nil {
		t.Cleanup(res.Stmts.Close)
	}
	return res, err, evs
}

func startOf(ev *replication.BinlogEvent) uint32 { return ev.Header.LogPos - ev.Header.EventSize }

// 流停住时，位置已经到了开始时最后一个文件的末尾：是真读完了，不是卡住。
func TestStalledAtEndIsCleanEnd(t *testing.T) {
	for _, mode := range []string{ModeSQL, ModeRollback} {
		res, err, _ := scanN(t, scenario(), baseSpec(mode), 0)
		if err != nil || res.Truncated != "" || res.Next != nil || res.Stmts.Len() != 3 {
			t.Errorf("%s: 读到末尾后空闲应算读完: err=%v trunc=%q next=%v n=%d lastpos=%d", mode, err, res.Truncated, res.Next, res.Stmts.Len(), res.LastPos)
		}
	}
}

// 流停住时位置还没到末尾：目标库卡住了，不能当成读完。
func TestStalledEarlySQLModeIsTruncated(t *testing.T) {
	res, err, _ := scanN(t, scenario(), baseSpec(ModeSQL), evAfterTrx1)
	if err != nil || res.Truncated != "stall" || res.Next != nil || res.Stmts.Len() != 2 {
		t.Fatalf("还原模式：保留已读到的部分并标明目标库卡住: err=%v trunc=%q next=%v n=%d", err, res.Truncated, res.Next, res.Stmts.Len())
	}
	if txt := truncText("stall", Limits{}, res); !strings.Contains(txt, "没有返回事件") || !strings.Contains(txt, "bin.000001:") {
		t.Errorf("提示里要写读到了哪里: %s", txt)
	}
}

func TestStalledEarlyRollbackSegmentsAtTransactionBoundary(t *testing.T) {
	// 卡在事务 1 提交之后：事务 1 完整保留，续接位置是事务 2 的起点
	res, err, evs := scanN(t, scenario(), baseSpec(ModeRollback), evAfterTrx1)
	want := Resume{File: "bin.000001", Pos: startOf(evs[evAfterTrx1])} // 事务 2 的 BEGIN
	if err != nil || res.Truncated != "stall" || res.Next == nil || *res.Next != want || res.Stmts.Len() != 2 || res.Trx != 1 {
		t.Fatalf("事务边界上卡住: err=%v trunc=%q next=%v want=%v n=%d trx=%d", err, res.Truncated, res.Next, want, res.Stmts.Len(), res.Trx)
	}
	// 卡在事务 1 中间：整个事务 1 丢掉，续接位置是它的起点；计数还原
	res, err, evs = scanN(t, scenario(), baseSpec(ModeRollback), evAfterInsert)
	want = Resume{File: "bin.000001", Pos: startOf(evs[2])} // 事务 1 的 BEGIN
	if err != nil || res.Next == nil || *res.Next != want || res.Stmts.Len() != 0 || res.Trx != 0 || res.Bytes != 0 || len(res.ByType) != 0 || !res.unitCut {
		t.Fatalf("事务中间卡住应丢掉没读完的事务: err=%v next=%v want=%v n=%d trx=%d bytes=%d bytype=%v", err, res.Next, want, res.Stmts.Len(), res.Trx, res.Bytes, res.ByType)
	}
}

func TestScanLimitSegments(t *testing.T) {
	// 扫描事件数上限：刚好在事务 1 提交之后到上限 → 本段含事务 1，续接事务 2
	sp := baseSpec(ModeRollback)
	sp.MaxScan = evAfterTrx1
	res, err, evs := scanN(t, scenario(), sp, 0)
	if err != nil || res.Truncated != "scan" || res.Next == nil || res.Next.Pos != startOf(evs[evAfterTrx1]) || res.Stmts.Len() != 2 {
		t.Fatalf("扫描上限（事务边界）: err=%v trunc=%q next=%v n=%d", err, res.Truncated, res.Next, res.Stmts.Len())
	}
	// 在事务中间到上限：丢掉没读完的事务 2（它的删除语句已经加进去了），本段仍含完整的事务 1
	sp.MaxScan = evAfterDeleteA
	res, err, evs = scanN(t, scenario(), sp, 0)
	if err != nil || res.Truncated != "scan" || res.Next == nil || res.Next.Pos != startOf(evs[evAfterTrx1]) || res.Stmts.Len() != 2 || res.Trx != 1 {
		t.Fatalf("扫描上限（事务中间）: err=%v trunc=%q next=%v n=%d trx=%d", err, res.Truncated, res.Next, res.Stmts.Len(), res.Trx)
	}
	if res.ByType[KindDelete] != 0 || res.ByType[KindInsert] != 1 || res.ByType[KindUpdate] != 1 {
		t.Errorf("被丢掉的事务的类型计数要还原: %v", res.ByType)
	}
	// 还原模式：到上限只截断，不分段
	sp = baseSpec(ModeSQL)
	sp.MaxScan = evAfterDeleteA
	res, _, _ = scanN(t, scenario(), sp, 0)
	if res.Truncated != "scan" || res.Next != nil || res.Stmts.Len() != 3 {
		t.Errorf("还原模式: trunc=%q next=%v n=%d", res.Truncated, res.Next, res.Stmts.Len())
	}
}

func TestBytesLimitSegmentsAndReleasesDroppedBytes(t *testing.T) {
	// 先量出事务 1 两条语句的体积，再把上限设成“刚好放下事务 1 + 事务 2 的一半”不可能的值：只放得下事务 1
	full, _, _ := scanN(t, scenario(), baseSpec(ModeRollback), 0)
	all := stmtsOf(full)
	if len(all) != 3 {
		t.Fatalf("n=%d", len(all))
	}
	trx1Bytes := all[1].size() + all[2].size() // 回滚顺序：[删除的回滚, 更新的回滚, 插入的回滚]，后两条是事务 1
	sp := baseSpec(ModeRollback)
	sp.MaxBytes = trx1Bytes + all[0].size() - 1 // 放不下事务 2 的那一条
	res, err, _ := scanN(t, scenario(), sp, 0)
	if err != nil || res.Truncated != "bytes" || res.Next == nil || res.Stmts.Len() != 2 || res.Bytes != trx1Bytes {
		t.Fatalf("体积上限: err=%v trunc=%q next=%v n=%d bytes=%d want=%d", err, res.Truncated, res.Next, res.Stmts.Len(), res.Bytes, trx1Bytes)
	}
}

// 不在事务里的单条语句（DDL）放不下：续接位置是它的起点，计数不重复。
func TestStandaloneDDLOverLimitSegments(t *testing.T) {
	b := newBin(t0)
	b.fde()
	b.query("shop", "ALTER TABLE orders ADD COLUMN x INT", t0+1)
	b.query("shop", "ALTER TABLE orders ADD COLUMN y INT", t0+2)
	sp := baseSpec(ModeRollback)
	sp.Types[KindDDL] = true
	sp.MaxRows = 1
	res, err, evs := scanN(t, b, sp, 0)
	if err != nil || res.Truncated != "rows" || res.Next == nil || res.Next.Pos != startOf(evs[3]) || res.Stmts.Len() != 1 || res.DDL != 1 {
		t.Fatalf("单条 DDL 放不下: err=%v trunc=%q next=%v n=%d ddl=%d", err, res.Truncated, res.Next, res.Stmts.Len(), res.DDL)
	}
}

// 回滚模式下不在事务里的行变更放不下：前面没有可用的起点，只能失败。
func TestRowsOutsideTransactionCannotSegment(t *testing.T) {
	b := newBin(t0)
	b.fde()
	b.tableMap(ordersTbl, t0+1)
	b.insert(t0+1, []any{1, "a", 1, nil})
	b.tableMap(ordersTbl, t0+1)
	b.insert(t0+1, []any{2, "b", 1, nil})
	sp := baseSpec(ModeRollback)
	sp.MaxRows = 1
	_, err, _ := scanN(t, b, sp, 0)
	var be *Error
	if !errors.As(err, &be) || be.Code != "limit_exceeded" || !strings.Contains(be.Message, "不在事务里") {
		t.Fatalf("不在事务里的行变更放不下应失败并说明原因: %v", err)
	}
}

func TestNormalizeNext(t *testing.T) {
	p := &plan{
		files: []LogFile{{Name: "bin.000001", Size: 1000}, {Name: "bin.000002", Size: 500}, {Name: "bin.000003", Size: 300}},
		spec:  scanSpec{LastFile: "bin.000003", LastSize: 300},
	}
	cases := []struct {
		name string
		in   Resume
		want *Resume
	}{
		{"文件中间原样", Resume{"bin.000001", 400}, &Resume{"bin.000001", 400}},
		{"正好在文件末尾：换下一个文件的开头", Resume{"bin.000001", 1000}, &Resume{"bin.000002", 4}},
		{"超过文件末尾也一样", Resume{"bin.000002", 600}, &Resume{"bin.000003", 4}},
		{"最后一个文件的末尾：没有后续", Resume{"bin.000003", 300}, nil},
		{"到了结束位置：没有后续", Resume{"bin.000003", 350}, nil},
		{"文件不在列表里：不改", Resume{"bin.000009", 10}, nil}, // 比结束文件还靠后
	}
	for _, c := range cases {
		got := normalizeNext(p, c.in)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%s: normalizeNext(%v) = %v，期望 %v", c.name, c.in, got, c.want)
		}
	}
	// 指定了结束位置：到了结束位置就没有后续
	p.spec = scanSpec{EndFile: "bin.000002", EndPos: 200}
	if got := normalizeNext(p, Resume{"bin.000002", 200}); got != nil {
		t.Errorf("到结束位置: %v", got)
	}
	if got := normalizeNext(p, Resume{"bin.000002", 100}); got == nil || got.Pos != 100 {
		t.Errorf("结束位置之前: %v", got)
	}
}

// go-mysql 收到非阻塞模式的 EOF 包时只写一条日志：日志处理器要把它接住；其它日志不影响。
func TestEOFHandlerCatchesLibraryLog(t *testing.T) {
	var flag atomic.Bool
	lg := slog.New(&eofHandler{flag: &flag})
	lg.Error("some error", "x", 1)
	lg.Info("retry sync")
	if flag.Load() {
		t.Fatal("无关日志不应置位")
	}
	lg.Info("receive EOF packet, no more binlog event now.")
	if !flag.Load() {
		t.Error("EOF 包的日志应置位")
	}
}

// 服务端发过 EOF 包：缓冲取空就是真读完（io.EOF）；没发过：空闲超时算 errStalled。
func TestSyncStreamIdleSemantics(t *testing.T) {
	ch := replication.NewBinlogStreamer()
	var flag atomic.Bool
	s := &syncStream{streamer: ch, idle: 30_000_000, eof: &flag} // 30ms
	if _, err := s.Next(context.Background()); !errors.Is(err, errStalled) {
		t.Fatalf("没收到 EOF 包的空闲应是 errStalled: %v", err)
	}
	flag.Store(true)
	if _, err := s.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("收到 EOF 包之后的空闲应是 io.EOF: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("取消要原样返回: %v", err)
	}
}
