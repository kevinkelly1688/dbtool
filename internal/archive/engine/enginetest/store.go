// @author: kevinkelly
// @license: Apache Licence
// @file: store.go
// @time: 2026/10/28

package enginetest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"dbtool/internal/archive/engine"
)

// MemStore 是内存里的 FileStore。
type MemStore struct {
	mu   sync.Mutex
	Log  *OpLog
	objs map[string][]byte

	// 钩子
	PutErr    func(key string, call int) error     // 返回错误 = 上传失败
	GetMangle func(key string, data []byte) []byte // 改写读回的内容（模拟回读校验失败）
	StatErr   func(key string) error               // 返回错误（非 NotExist）
	FreeBytes int64                                // Free 的返回值
	FreeOK    bool                                 // Free 的 ok
	TestErr   error                                // Test 的返回值
	Puts      int
	PutKeys   []string
}

func NewMemStore(log *OpLog) *MemStore {
	return &MemStore{Log: log, objs: map[string][]byte{}, FreeBytes: 1 << 40, FreeOK: true}
}

func (m *MemStore) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if size >= 0 && int64(len(b)) != size {
		return fmt.Errorf("大小不一致 %d/%d", len(b), size)
	}
	m.mu.Lock()
	m.Puts++
	call := m.Puts
	hook := m.PutErr
	m.mu.Unlock()
	if hook != nil {
		if err := hook(key, call); err != nil {
			return err
		}
	}
	m.mu.Lock()
	m.objs[key] = b
	m.PutKeys = append(m.PutKeys, key)
	m.mu.Unlock()
	m.Log.add("file.put", nil, len(b))
	return nil
}

func (m *MemStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	b, ok := m.objs[key]
	mangle := m.GetMangle
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%s：%w", key, engine.ErrNotExist)
	}
	if mangle != nil {
		b = mangle(key, append([]byte(nil), b...))
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *MemStore) Stat(ctx context.Context, key string) (engine.FileInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.StatErr != nil {
		if err := m.StatErr(key); err != nil {
			return engine.FileInfo{}, err
		}
	}
	b, ok := m.objs[key]
	if !ok {
		return engine.FileInfo{}, fmt.Errorf("%s：%w", key, engine.ErrNotExist)
	}
	return engine.FileInfo{Key: key, Size: int64(len(b))}, nil
}

func (m *MemStore) Delete(ctx context.Context, key string) error {
	m.mu.Lock()
	delete(m.objs, key)
	m.mu.Unlock()
	return nil
}

func (m *MemStore) Test(ctx context.Context) (time.Duration, error) {
	return 3 * time.Millisecond, m.TestErr
}

func (m *MemStore) Free(ctx context.Context) (int64, bool, error) { return m.FreeBytes, m.FreeOK, nil }

// Object 返回对象内容（不存在时 ok=false）。
func (m *MemStore) Object(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	return append([]byte(nil), b...), ok
}

// Keys 返回所有对象的 Key（排序）。
func (m *MemStore) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.objs))
	for k := range m.objs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var _ engine.FileStore = (*MemStore)(nil)

// MemProbe 是可编程的负载探针。字段为 nil 时返回“正常”。call 从 1 开始计数。
type MemProbe struct {
	mu         sync.Mutex
	Threads    func(call int) (int, error)
	Lag        func(call int) (sec int, has bool, err error)
	Disk       func(call int) (pct float64, ok bool, err error)
	nT, nL, nD int
	MaxThreads int // 观察到的最大 Threads 返回值
}

func (p *MemProbe) ThreadsRunning(ctx context.Context) (int, error) {
	p.mu.Lock()
	p.nT++
	call, f := p.nT, p.Threads
	p.mu.Unlock()
	if f == nil {
		return 3, nil
	}
	return f(call)
}

func (p *MemProbe) ReplicaLag(ctx context.Context) (int, bool, error) {
	p.mu.Lock()
	p.nL++
	call, f := p.nL, p.Lag
	p.mu.Unlock()
	if f == nil {
		return 0, false, nil
	}
	return f(call)
}

func (p *MemProbe) DiskFreePct(ctx context.Context) (float64, bool, error) {
	p.mu.Lock()
	p.nD++
	call, f := p.nD, p.Disk
	p.mu.Unlock()
	if f == nil {
		return 70, true, nil
	}
	return f(call)
}

// Calls 返回三类探测被调用的次数。
func (p *MemProbe) Calls() (threads, lag, disk int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.nT, p.nL, p.nD
}

var _ engine.LoadProbe = (*MemProbe)(nil)
