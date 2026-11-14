// @author: kevinkelly
// @license: Apache Licence
// @file: stmtstore_test.go
// @time: 2026/10/28

package binlog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mkStmt(i int) Stmt {
	return Stmt{Trx: i/3 + 1, Time: "2026-10-19 10:00:00", File: "bin.000001", Pos: uint32(100 + i*10), EndPos: uint32(105 + i*10),
		Schema: "shop", Table: "orders", Type: KindInsert, SQL: fmt.Sprintf("DELETE FROM `shop`.`orders` WHERE `id` = %d;", i), NoRollback: i%7 == 0}
}

func collect(t *testing.T, each func(func(Stmt) error) error) []Stmt {
	t.Helper()
	var out []Stmt
	if err := each(func(s Stmt) error { out = append(out, s); return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

func spillFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, "*.spill"))
	return m
}

func same(a, b Stmt) bool { a.Seq, b.Seq = 0, 0; return a == b }

func TestStmtCodecRoundTrip(t *testing.T) {
	for _, st := range []Stmt{
		{},
		mkStmt(1),
		{Trx: 1 << 20, Time: "t", File: "f", Pos: 1<<32 - 1, EndPos: 1<<32 - 1, Schema: "库", Table: "表\x00\n", Type: KindDDL, SQL: strings.Repeat("中文'\\\"", 1000), NoRollback: true},
	} {
		got, err := decodeStmt(encodeStmt(nil, &st))
		if err != nil || !same(got, st) {
			t.Errorf("往返不一致: %v\n%+v\n%+v", err, st, got)
		}
	}
	enc := encodeStmt(nil, &Stmt{SQL: "select 1"})
	for _, bad := range [][]byte{nil, enc[:len(enc)-1], enc[:3], append(append([]byte{}, enc...), 9)} {
		if _, err := decodeStmt(bad); !errors.Is(err, errCorrupt) {
			t.Errorf("损坏的记录应报 errCorrupt: %v", err)
		}
	}
}

func TestStmtStoreMemoryOnly(t *testing.T) {
	s := newStmtStore("", 0) // 没有临时目录：不落盘
	for i := 0; i < 1000; i++ {
		if err := s.Append(mkStmt(i)); err != nil {
			t.Fatal(err)
		}
	}
	if s.Len() != 1000 || s.OnDisk() || s.DiskBytes() != 0 || s.MemBytes() <= 0 {
		t.Fatalf("内存模式: len=%d disk=%v", s.Len(), s.OnDisk())
	}
	p, _ := s.Page(10, 3, false)
	if len(p) != 3 || p[0].Seq != 11 || !same(p[0], mkStmt(10)) {
		t.Errorf("正序分页: %+v", p)
	}
	p, _ = s.Page(0, 3, true) // 倒序：最后存入的在最前
	if len(p) != 3 || p[0].Seq != 1 || !same(p[0], mkStmt(999)) || !same(p[2], mkStmt(997)) {
		t.Errorf("倒序分页: %+v", p)
	}
	if p, _ = s.Page(998, 50, true); len(p) != 2 || !same(p[1], mkStmt(0)) || p[1].Seq != 1000 {
		t.Errorf("倒序最后一页: %+v", p)
	}
	if p, _ = s.Page(1000, 5, false); len(p) != 0 {
		t.Errorf("越界返回空: %+v", p)
	}
	before := s.MemBytes()
	if err := s.Truncate(400); err != nil || s.Len() != 400 || s.MemBytes() >= before {
		t.Errorf("截断: %v len=%d mem=%d→%d", err, s.Len(), before, s.MemBytes())
	}
	all := collect(t, s.EachReverse)
	if len(all) != 400 || !same(all[0], mkStmt(399)) || !same(all[399], mkStmt(0)) {
		t.Errorf("倒序遍历: %d", len(all))
	}
}

func TestStmtStoreSpillsAndPages(t *testing.T) {
	dir := t.TempDir()
	s := newStmtStore(dir, 2<<10) // 2 KB 就转存，用很少的数据覆盖磁盘路径
	t.Cleanup(s.Close)
	const n = 1300 // 跨过 5 个稀疏索引点
	for i := 0; i < n; i++ {
		if err := s.Append(mkStmt(i)); err != nil {
			t.Fatal(err)
		}
	}
	if !s.OnDisk() || s.MemBytes() != 0 || len(spillFiles(t, dir)) != 1 {
		t.Fatalf("应已转存到磁盘: disk=%v mem=%d files=%v", s.OnDisk(), s.MemBytes(), spillFiles(t, dir))
	}
	if err := s.Seal(); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(spillFiles(t, dir)[0]); fi.Size() != s.DiskBytes() {
		t.Errorf("文件大小 %d 与记录的 %d 不一致", fi.Size(), s.DiskBytes())
	}
	fwd := collect(t, s.EachForward)
	rev := collect(t, s.EachReverse)
	if len(fwd) != n || len(rev) != n {
		t.Fatalf("遍历条数: %d %d", len(fwd), len(rev))
	}
	for i := 0; i < n; i++ {
		if !same(fwd[i], mkStmt(i)) || !same(rev[i], mkStmt(n-1-i)) {
			t.Fatalf("第 %d 条不一致", i)
		}
	}
	for _, c := range []struct{ off, lim int }{{0, 5}, {250, 10}, {255, 2}, {256, 1}, {511, 3}, {512, 300}, {1295, 50}, {1299, 1}} {
		p, err := s.Page(c.off, c.lim, false)
		if err != nil {
			t.Fatal(err)
		}
		for i, st := range p {
			if st.Seq != c.off+i+1 || !same(st, mkStmt(c.off+i)) {
				t.Fatalf("正序分页 %+v 第 %d 条不对: %+v", c, i, st)
			}
		}
		if want := min(c.lim, n-c.off); len(p) != want {
			t.Errorf("正序分页 %+v 条数 %d 期望 %d", c, len(p), want)
		}
		pr, err := s.Page(c.off, c.lim, true)
		if err != nil {
			t.Fatal(err)
		}
		for i, st := range pr {
			if st.Seq != c.off+i+1 || !same(st, mkStmt(n-1-(c.off+i))) {
				t.Fatalf("倒序分页 %+v 第 %d 条不对: %+v", c, i, st)
			}
		}
	}
}

func TestStmtStoreTruncateOnDiskThenAppend(t *testing.T) {
	dir := t.TempDir()
	s := newStmtStore(dir, 1<<10)
	t.Cleanup(s.Close)
	for i := 0; i < 700; i++ {
		if err := s.Append(mkStmt(i)); err != nil {
			t.Fatal(err)
		}
	}
	for _, keep := range []int{600, 513, 512, 300, 256, 255, 1, 0} {
		if err := s.Truncate(keep); err != nil || s.Len() != keep {
			t.Fatalf("Truncate(%d): %v len=%d", keep, err, s.Len())
		}
		// 截断之后继续追加（事务写一半被丢掉后，下一个事务接着写）
		for i := keep; i < keep+300; i++ {
			if err := s.Append(mkStmt(1000 + i)); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Truncate(keep + 300); err != nil {
			t.Fatal(err)
		}
		if p, err := s.Page(keep, 300, false); err != nil || len(p) != 300 || !same(p[0], mkStmt(1000+keep)) || !same(p[299], mkStmt(1000+keep+299)) {
			t.Fatalf("keep=%d 截断后追加的内容不对: %v", keep, err)
		}
		if err := s.Truncate(keep); err != nil { // 回到 keep，为下一轮准备
			t.Fatal(err)
		}
		if keep > 0 {
			if p, _ := s.Page(0, 1, false); len(p) != 1 || !same(p[0], mkStmt(0)) {
				t.Fatalf("keep=%d 前面的内容被破坏", keep)
			}
		}
	}
	if s.Len() != 0 {
		t.Fatalf("len=%d", s.Len())
	}
}

func TestStmtStoreSpillDuringTruncateMark(t *testing.T) {
	// 记下条数（相当于事务开头的标记）之后才转存，再按标记截断
	dir := t.TempDir()
	s := newStmtStore(dir, 4<<10)
	t.Cleanup(s.Close)
	for i := 0; i < 10; i++ {
		_ = s.Append(mkStmt(i))
	}
	mark := s.Len()
	if s.OnDisk() {
		t.Fatal("前 10 条不该转存")
	}
	for i := 10; i < 400; i++ {
		_ = s.Append(mkStmt(i))
	}
	if !s.OnDisk() {
		t.Fatal("之后应已转存")
	}
	if err := s.Truncate(mark); err != nil || s.Len() != mark {
		t.Fatalf("按标记截断: %v %d", err, s.Len())
	}
	_ = s.Seal()
	got := collect(t, s.EachForward)
	if len(got) != 10 || !same(got[9], mkStmt(9)) {
		t.Errorf("截断后: %d", len(got))
	}
}

func TestStmtStoreBigRecordsLargerThanWindow(t *testing.T) {
	dir := t.TempDir()
	s := newStmtStore(dir, 1<<10)
	t.Cleanup(s.Close)
	big := func(c byte, n int) string { return strings.Repeat(string(c), n) }
	sizes := []int{10, readWindow + 123, 20, 2*readWindow + 7, 30, 3 * readWindow}
	for i, n := range sizes {
		st := mkStmt(i)
		st.SQL = big(byte('a'+i), n)
		if err := s.Append(st); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Seal()
	rev := collect(t, s.EachReverse)
	if len(rev) != len(sizes) {
		t.Fatalf("条数 %d", len(rev))
	}
	for i, st := range rev {
		k := len(sizes) - 1 - i
		if len(st.SQL) != sizes[k] || st.SQL[0] != byte('a'+k) || st.SQL[len(st.SQL)-1] != byte('a'+k) {
			t.Errorf("倒序第 %d 条（原第 %d 条）内容不对：长度 %d", i, k, len(st.SQL))
		}
	}
}

func TestStmtStoreCloseDeletesFileAndRefsDeferIt(t *testing.T) {
	dir := t.TempDir()
	s := newStmtStore(dir, 1<<10)
	t.Cleanup(s.Close)
	for i := 0; i < 300; i++ {
		_ = s.Append(mkStmt(i))
	}
	_ = s.Seal()
	if len(spillFiles(t, dir)) != 1 {
		t.Fatal("应有一个临时文件")
	}
	if !s.Acquire() {
		t.Fatal("Acquire")
	}
	s.Close()
	if len(spillFiles(t, dir)) != 1 {
		t.Error("读取还没结束，文件不能删")
	}
	if s.Acquire() {
		t.Error("已关闭的结果不能再被读取")
	}
	if _, err := s.Page(0, 1, false); !errors.Is(err, errStoreClosed) {
		t.Errorf("已关闭: %v", err)
	}
	s.Release()
	if len(spillFiles(t, dir)) != 0 {
		t.Error("最后一个读取结束后应删掉文件")
	}
	s.Close() // 可重复调用
}

func TestStmtStoreWriteErrorIsSticky(t *testing.T) {
	// 临时目录是个文件：创建临时文件失败，错误要带目录并一直保持
	f := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newStmtStore(f, 1<<10)
	t.Cleanup(s.Close)
	var err error
	for i := 0; i < 100 && err == nil; i++ {
		err = s.Append(mkStmt(i))
	}
	if err == nil || !strings.Contains(err.Error(), f) {
		t.Fatalf("应报创建临时目录 / 文件失败并带上路径: %v", err)
	}
	if err2 := s.Append(mkStmt(1)); err2 == nil {
		t.Error("出错之后的写入要一直失败")
	}
}

// 条数多时的遍历与分页：10 万条语句落盘，正 / 倒序遍历条数正确，抽查分页内容，整个过程应在几秒内完成。
func TestStmtStoreManyRecords(t *testing.T) {
	if testing.Short() {
		t.Skip("压量用例，-short 时跳过")
	}
	const n = 100_000
	dir := t.TempDir()
	s := newStmtStore(dir, 1<<20)
	t.Cleanup(s.Close)
	for i := 0; i < n; i++ {
		if err := s.Append(mkStmt(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Seal(); err != nil || !s.OnDisk() || s.Len() != n {
		t.Fatalf("seal: %v disk=%v len=%d", err, s.OnDisk(), s.Len())
	}
	cnt, last := 0, -1
	if err := s.EachReverse(func(st Stmt) error {
		if want := mkStmt(n - 1 - cnt); !same(st, want) {
			t.Fatalf("倒序第 %d 条不对", cnt)
		}
		cnt++
		last = int(st.Pos)
		return nil
	}); err != nil || cnt != n || last != 100 {
		t.Fatalf("倒序遍历: %v cnt=%d last=%d", err, cnt, last)
	}
	cnt = 0
	if err := s.EachForward(func(Stmt) error { cnt++; return nil }); err != nil || cnt != n {
		t.Fatalf("正序遍历: %v cnt=%d", err, cnt)
	}
	for _, off := range []int{0, 255, 256, 54321, n - 200} {
		p, err := s.Page(off, 200, true)
		if err != nil || len(p) != min(200, n-off) || !same(p[0], mkStmt(n-1-off)) || p[0].Seq != off+1 {
			t.Fatalf("倒序分页 off=%d: %v len=%d", off, err, len(p))
		}
	}
}
