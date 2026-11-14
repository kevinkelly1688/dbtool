// @author: kevinkelly
// @license: Apache Licence
// @file: stmtstore.go
// @time: 2026/10/28

package binlog

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// 结果语句的存放：按扫描顺序（正序）保存。小结果放内存；超过 spillThreshold 后转存到临时文件，内存里只留一个稀疏索引，
// 所以结果可以比内存大得多。回滚结果“后发生的先撤销”的倒序只是读取方式（Page / EachReverse），不改变存放顺序。
//
// 文件里每条记录是 [4 字节长度][正文][4 字节长度]：前面的长度用来正向读，后面的长度用来从文件尾倒着读。
// 扫描协程写（Append / Truncate），扫描结束后 Seal；接口协程只在 Seal 之后读。临时文件含业务数据：目录 0700、文件 0600，结果过期或任务失败时立刻删除。

const (
	// spillThreshold 是单个任务的结果超过多大就转存到磁盘。与原来的“每个任务最多占 8 MB 内存”一致，所以内存预算不变。
	spillThreshold = 8 << 20
	idxStep        = 256     // 稀疏索引：每 idxStep 条语句记一次文件偏移
	writeBufSize   = 1 << 20 // 写缓冲
	readWindow     = 4 << 20 // 倒着读时每次读入的窗口
)

// maxRecord 是单条记录的长度上限：只用来识别损坏的文件，避免按一个错误的长度去分配内存。
const maxRecord = 1 << 30

var errStoreClosed = errors.New("结果已被清理")

type stmtStore struct {
	dir       string // 临时文件目录；空 = 不落盘（只放内存）
	threshold int

	mu       sync.Mutex
	n        int
	mem      []Stmt
	memBytes int

	f         *os.File
	path      string
	wbuf      []byte
	flushed   int64   // 已写入文件的字节数
	diskBytes int64   // 文件大小（含写缓冲里的）
	idx       []int64 // idx[k] = 第 k*idxStep 条语句的文件偏移
	err       error   // 写文件失败（之后所有写入都返回它）
	sealed    bool

	refs   int
	closed bool
}

func newStmtStore(dir string, threshold int) *stmtStore {
	if threshold <= 0 {
		threshold = spillThreshold
	}
	return &stmtStore{dir: dir, threshold: threshold}
}

// Len 返回语句条数。
func (s *stmtStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// MemBytes 是内存里语句的估算体积；DiskBytes 是临时文件的大小。
func (s *stmtStore) MemBytes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.memBytes
}

func (s *stmtStore) DiskBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.diskBytes
}

// OnDisk 报告结果是否已转存到磁盘。
func (s *stmtStore) OnDisk() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f != nil
}

// ---- 编码 ----

func putStr(b []byte, s string) []byte {
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

func encodeStmt(dst []byte, st *Stmt) []byte {
	dst = binary.AppendUvarint(dst, uint64(st.Trx))
	dst = putStr(dst, st.Time)
	dst = putStr(dst, st.File)
	dst = binary.AppendUvarint(dst, uint64(st.Pos))
	dst = binary.AppendUvarint(dst, uint64(st.EndPos))
	dst = putStr(dst, st.Schema)
	dst = putStr(dst, st.Table)
	dst = putStr(dst, st.Type)
	dst = putStr(dst, st.SQL)
	if st.NoRollback {
		return append(dst, 1)
	}
	return append(dst, 0)
}

var errCorrupt = errors.New("结果临时文件损坏")

type decoder struct {
	b   []byte
	err error
}

func (d *decoder) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.err = errCorrupt
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *decoder) str() string {
	n := d.uvarint()
	if d.err != nil {
		return ""
	}
	if n > uint64(len(d.b)) {
		d.err = errCorrupt
		return ""
	}
	s := string(d.b[:n])
	d.b = d.b[n:]
	return s
}

func decodeStmt(b []byte) (Stmt, error) {
	d := decoder{b: b}
	var st Stmt
	st.Trx = int(d.uvarint())
	st.Time = d.str()
	st.File = d.str()
	st.Pos = uint32(d.uvarint())
	st.EndPos = uint32(d.uvarint())
	st.Schema = d.str()
	st.Table = d.str()
	st.Type = d.str()
	st.SQL = d.str()
	if d.err == nil && len(d.b) != 1 {
		d.err = errCorrupt
	}
	if d.err != nil {
		return Stmt{}, d.err
	}
	st.NoRollback = d.b[0] == 1
	return st, nil
}

// ---- 写 ----

// Append 追加一条语句（扫描协程调用）。内存里的体积超过阈值就转存磁盘。
func (s *stmtStore) Append(st Stmt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if s.sealed || s.closed {
		return errStoreClosed
	}
	if s.f == nil {
		s.mem = append(s.mem, st)
		s.memBytes += st.size()
		s.n++
		if s.dir != "" && s.memBytes > s.threshold {
			return s.spillLocked()
		}
		return nil
	}
	return s.writeLocked(&st)
}

// spillLocked 把内存里的语句全部写进新建的临时文件，然后释放内存。
func (s *stmtStore) spillLocked() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		s.err = fmt.Errorf("创建结果临时目录 %s 失败：%w", s.dir, err)
		return s.err
	}
	f, err := os.CreateTemp(s.dir, "binlog-*.spill")
	if err != nil {
		s.err = fmt.Errorf("创建结果临时文件失败（目录 %s）：%w", s.dir, err)
		return s.err
	}
	s.f, s.path = f, f.Name()
	mem := s.mem
	s.mem, s.memBytes, s.n = nil, 0, 0
	for i := range mem {
		if err := s.writeLocked(&mem[i]); err != nil {
			return err
		}
	}
	return nil
}

// writeLocked 把一条语句追加到写缓冲（满了就刷到文件），并维护稀疏索引。
func (s *stmtStore) writeLocked(st *Stmt) error {
	off := s.diskBytes
	if s.n%idxStep == 0 {
		s.idx = append(s.idx, off)
	}
	start := len(s.wbuf)
	s.wbuf = append(s.wbuf, 0, 0, 0, 0)
	s.wbuf = encodeStmt(s.wbuf, st)
	plen := uint32(len(s.wbuf) - start - 4)
	binary.LittleEndian.PutUint32(s.wbuf[start:], plen)
	s.wbuf = binary.LittleEndian.AppendUint32(s.wbuf, plen)
	s.diskBytes += int64(len(s.wbuf) - start)
	s.n++
	if len(s.wbuf) >= writeBufSize {
		return s.flushLocked()
	}
	return nil
}

func (s *stmtStore) flushLocked() error {
	if s.err != nil {
		return s.err
	}
	if len(s.wbuf) == 0 || s.f == nil {
		return nil
	}
	if _, err := s.f.WriteAt(s.wbuf, s.flushed); err != nil {
		s.err = fmt.Errorf("写结果临时文件失败（磁盘空间不足或目录不可写）：%w", err)
		return s.err
	}
	s.flushed += int64(len(s.wbuf))
	s.wbuf = s.wbuf[:0]
	return nil
}

// Truncate 丢掉第 n 条（从 0 数）及之后的语句：回滚分段时丢掉最后那个没写完的事务。
func (s *stmtStore) Truncate(n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if s.sealed || s.closed {
		return errStoreClosed
	}
	if n < 0 {
		n = 0
	}
	if n >= s.n {
		return nil
	}
	if s.f == nil {
		for i := n; i < len(s.mem); i++ {
			s.memBytes -= s.mem[i].size()
			s.mem[i] = Stmt{}
		}
		s.mem = s.mem[:n]
		s.n = n
		return nil
	}
	if err := s.flushLocked(); err != nil {
		return err
	}
	off, err := s.offsetOfLocked(n)
	if err != nil {
		s.err = err
		return err
	}
	if err := s.f.Truncate(off); err != nil {
		s.err = fmt.Errorf("截断结果临时文件失败：%w", err)
		return s.err
	}
	s.flushed, s.diskBytes, s.n = off, off, n
	s.wbuf = s.wbuf[:0]
	s.idx = s.idx[:(n+idxStep-1)/idxStep]
	return nil
}

// Seal 标记扫描结束：把缓冲刷进文件，之后只读。
func (s *stmtStore) Seal() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errStoreClosed
	}
	s.sealed = true
	return s.flushLocked()
}

// ---- 读 ----

// offsetOfLocked 返回第 n 条语句（n ≤ s.n）在文件里的偏移。调用方已刷新写缓冲。
func (s *stmtStore) offsetOfLocked(n int) (int64, error) {
	if n >= s.n {
		return s.diskBytes, nil
	}
	k := n / idxStep
	off := s.idx[k]
	for i := k * idxStep; i < n; i++ {
		var hdr [4]byte
		if _, err := s.f.ReadAt(hdr[:], off); err != nil {
			return 0, fmt.Errorf("读结果临时文件失败：%w", err)
		}
		off += 8 + int64(binary.LittleEndian.Uint32(hdr[:]))
	}
	return off, nil
}

// readForward 从偏移 off 起顺序读 count 条记录。
func readForward(r io.Reader, count int, fn func(Stmt) error) error {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 256<<10)
	}
	var hdr [4]byte
	var buf []byte
	for i := 0; i < count; i++ {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return fmt.Errorf("读结果临时文件失败：%w", err)
		}
		plen := int(binary.LittleEndian.Uint32(hdr[:]))
		if plen > maxRecord {
			return errCorrupt
		}
		if cap(buf) < plen {
			buf = make([]byte, plen)
		}
		buf = buf[:plen]
		if _, err := io.ReadFull(br, buf); err != nil {
			return fmt.Errorf("读结果临时文件失败：%w", err)
		}
		if _, err := io.ReadFull(br, hdr[:]); err != nil { // 尾部的长度，丢掉
			return fmt.Errorf("读结果临时文件失败：%w", err)
		}
		st, err := decodeStmt(buf)
		if err != nil {
			return err
		}
		if err := fn(st); err != nil {
			return err
		}
	}
	return nil
}

// Acquire / Release 让读取期间文件不会被清理掉：Close 时还有读取在进行，等最后一个 Release 再删文件。
func (s *stmtStore) Acquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.refs++
	return true
}

func (s *stmtStore) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refs--
	if s.closed && s.refs <= 0 {
		s.destroyLocked()
	}
}

// Close 清理结果：删除临时文件、释放内存。可重复调用。
func (s *stmtStore) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.refs <= 0 {
		s.destroyLocked()
	}
}

func (s *stmtStore) destroyLocked() {
	s.mem, s.memBytes, s.wbuf, s.idx = nil, 0, nil, nil
	if s.f != nil {
		_ = s.f.Close()
		_ = os.Remove(s.path)
		s.f = nil
	}
}

// Page 返回一页结果。reverse=true 时按“最后存入的在最前”的顺序（回滚结果的执行顺序）；返回的 Seq 从 offset+1 起连续编号。
func (s *stmtStore) Page(offset, limit int, reverse bool) ([]Stmt, error) {
	if !s.Acquire() {
		return nil, errStoreClosed
	}
	defer s.Release()
	s.mu.Lock()
	total := s.n
	if offset >= total || limit <= 0 {
		s.mu.Unlock()
		return []Stmt{}, nil
	}
	count := min(limit, total-offset)
	from := offset
	if reverse {
		from = total - offset - count
	}
	out := make([]Stmt, 0, count)
	if s.f == nil {
		out = append(out, s.mem[from:from+count]...)
		s.mu.Unlock()
	} else {
		if err := s.flushLocked(); err != nil {
			s.mu.Unlock()
			return nil, err
		}
		off, err := s.offsetOfLocked(from)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		r := io.NewSectionReader(s.f, off, s.diskBytes-off)
		s.mu.Unlock() // 文件已封存或只在末尾追加，读取不需要持锁
		if err := readForward(r, count, func(st Stmt) error { out = append(out, st); return nil }); err != nil {
			return nil, err
		}
	}
	if reverse {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	for i := range out {
		out[i].Seq = offset + i + 1
	}
	return out, nil
}

// EachForward 按存入顺序遍历全部语句（封存之后调用）。
func (s *stmtStore) EachForward(fn func(Stmt) error) error {
	if !s.Acquire() {
		return errStoreClosed
	}
	defer s.Release()
	s.mu.Lock()
	n := s.n
	if s.f == nil {
		mem := s.mem[:n:n]
		s.mu.Unlock()
		for _, st := range mem {
			if err := fn(st); err != nil {
				return err
			}
		}
		return nil
	}
	if err := s.flushLocked(); err != nil {
		s.mu.Unlock()
		return err
	}
	r := io.NewSectionReader(s.f, 0, s.diskBytes)
	s.mu.Unlock()
	return readForward(r, n, fn)
}

// EachReverse 按存入的相反顺序遍历全部语句（封存之后调用）：回滚文件按这个顺序输出。磁盘上用文件尾的长度倒着读，按窗口批量读入。
func (s *stmtStore) EachReverse(fn func(Stmt) error) error {
	if !s.Acquire() {
		return errStoreClosed
	}
	defer s.Release()
	s.mu.Lock()
	n := s.n
	if s.f == nil {
		mem := s.mem[:n:n]
		s.mu.Unlock()
		for i := len(mem) - 1; i >= 0; i-- {
			if err := fn(mem[i]); err != nil {
				return err
			}
		}
		return nil
	}
	if err := s.flushLocked(); err != nil {
		s.mu.Unlock()
		return err
	}
	f, end := s.f, s.diskBytes
	s.mu.Unlock()

	var buf []byte
	var bufStart int64
	// ensure 保证 [lo, hi) 在窗口里（hi 一定是当前读到的位置，所以窗口总是向前一个文件尾对齐）。
	ensure := func(lo, hi int64) error {
		if lo >= bufStart && hi <= bufStart+int64(len(buf)) {
			return nil
		}
		start := max(hi-max(int64(readWindow), hi-lo), 0)
		if cap(buf) < int(hi-start) {
			buf = make([]byte, hi-start)
		}
		buf = buf[:hi-start]
		if n, err := f.ReadAt(buf, start); err != nil && !(errors.Is(err, io.EOF) && n == len(buf)) {
			return fmt.Errorf("读结果临时文件失败：%w", err)
		}
		bufStart = start
		return nil
	}
	for i := 0; i < n; i++ {
		if end < 8 {
			return errCorrupt
		}
		if err := ensure(end-4, end); err != nil {
			return err
		}
		plen := int64(binary.LittleEndian.Uint32(buf[end-4-bufStart:]))
		recStart := end - 8 - plen
		if recStart < 0 || plen > maxRecord {
			return errCorrupt
		}
		if err := ensure(recStart, end); err != nil {
			return err
		}
		st, err := decodeStmt(buf[recStart+4-bufStart : recStart+4+plen-bufStart])
		if err != nil {
			return err
		}
		if err := fn(st); err != nil {
			return err
		}
		end = recStart
	}
	return nil
}
