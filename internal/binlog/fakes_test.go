// @author: kevinkelly
// @license: Apache Licence
// @file: fakes_test.go
// @time: 2026/10/28

package binlog

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/replication"
	"gorm.io/gorm"

	"dbtool/internal/audit"
	"dbtool/internal/btnperm"
	"dbtool/internal/engine"
	"dbtool/internal/model"
)

// ---- 假仓库 ----

type fakeRepo struct {
	instances []model.Instance
	perms     map[uint]map[string]bool
	groupInst map[uint]map[uint]bool
	cfg       map[string]string // 系统设置（binlog.max_rows 等）；没设的项读不到，服务用默认值
}

func (r *fakeRepo) ConfigValue(item string) (string, bool) {
	v, ok := r.cfg[item]
	return v, ok
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
func (r *fakeRepo) EngineConn(inst *model.Instance) engine.Conn {
	c := engine.Conn{Host: inst.Host, Port: inst.Port, User: "dbuser", Password: "s3cretPW"}
	if inst.TunnelID != nil { // 同 store.Store.EngineConn：配置了隧道的实例改写为本地转发器地址
		c.Host, c.Port = "127.0.0.1", 45678
	}
	return c
}
func (r *fakeRepo) HasButton(uid uint, code string) (bool, error) {
	return r.perms[uid][code] || btnperm.IsOpen(code), nil
}
func (r *fakeRepo) UserInstanceIDs(uid uint) (map[uint]bool, error) { return r.groupInst[uid], nil }

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		instances: []model.Instance{
			{ID: 1, InstanceName: "mysql-a", DBType: "mysql", Type: "master", Host: "10.0.0.1", Port: 3306},
			{ID: 2, InstanceName: "mysql-b", DBType: "mysql", Type: "slave", Host: "10.0.0.2", Port: 3306},
			{ID: 3, InstanceName: "mssql-c", DBType: "mssql", Host: "10.0.0.3", Port: 1433},
			{ID: 4, InstanceName: "mysql-tunnel", DBType: "mysql", Host: "10.0.0.4", Port: 3306, TunnelID: ptrUint(9)},
		},
		perms:     map[uint]map[string]bool{},
		groupInst: map[uint]map[uint]bool{2: {1: true, 3: true}},
	}
}

func ptrUint(v uint) *uint { return &v }

// ---- 假审计 ----

type fakeAudit struct {
	mu   sync.Mutex
	list []model.AuditEntry
	err  error
}

func (f *fakeAudit) Record(e *model.AuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.list = append(f.list, *e)
	return nil
}

func (f *fakeAudit) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, e := range f.list {
		out = append(out, e.Action)
	}
	return out
}

func (f *fakeAudit) last() model.AuditEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.list) == 0 {
		return model.AuditEntry{}
	}
	return f.list[len(f.list)-1]
}

// ---- 假来源 ----

type fakeSource struct {
	mu         sync.Mutex
	info       ServerInfo
	events     []*replication.BinlogEvent
	meta       *fakeMeta
	dbs        []string
	connectErr error
	infoErr    error
	streamErr  error
	block      chan struct{} // 非空时流在 Next 里阻塞，直到关闭或 ctx 结束
	connects   int
	conns      []engine.Conn // 每次 Connect 收到的连接信息
	sessions   int
	closed     int
	streams    int
}

func newFakeSource(b *binBuilder, t testing.TB) *fakeSource {
	evs := append([]*replication.BinlogEvent{rotateEvent(t, "bin.000001")}, b.events(t)...)
	return &fakeSource{
		info: ServerInfo{Version: "8.0.36", Flavor: "mysql", LogBin: true, Format: "ROW", RowImage: "FULL", TZOffset: 0,
			Files: []LogFile{{Name: "bin.000001", Size: int64(b.pos)}, {Name: "bin.000002", Size: 1000}}},
		events: evs, meta: newFakeMeta(), dbs: []string{"shop", "report", "mysql", "information_schema", "hidden"},
	}
}

func (f *fakeSource) Connect(ctx context.Context, c engine.Conn) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connects++
	f.conns = append(f.conns, c)
	if f.connectErr != nil {
		return nil, f.connectErr
	}
	f.sessions++
	return &fakeSession{f: f}, nil
}

func (f *fakeSource) openSessions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions - f.closed
}

type fakeSession struct{ f *fakeSource }

func (s *fakeSession) Columns(ctx context.Context, schema, table string) ([]colMeta, error) {
	return s.f.meta.Columns(ctx, schema, table)
}

func (s *fakeSession) Info(ctx context.Context) (*ServerInfo, error) {
	if s.f.infoErr != nil {
		return nil, s.f.infoErr
	}
	in := s.f.info
	in.Files = append([]LogFile(nil), s.f.info.Files...)
	return &in, nil
}

func (s *fakeSession) Databases(ctx context.Context) ([]string, error) {
	return append([]string(nil), s.f.dbs...), nil
}

func (s *fakeSession) Stream(ctx context.Context, file string, pos uint32, loc *time.Location) (EventStream, error) {
	s.f.mu.Lock()
	s.f.streams++
	s.f.mu.Unlock()
	if s.f.streamErr != nil {
		return nil, s.f.streamErr
	}
	// 和真实的 binlog dump 一样：从 pos 开始发（起点在 pos 之前的事件不发；文件头的 FORMAT_DESCRIPTION / ROTATE 照发）
	var evs []*replication.BinlogEvent
	for _, e := range s.f.events {
		h := e.Header
		if pos > 4 && h.LogPos != 0 && h.LogPos-h.EventSize < pos &&
			h.EventType != replication.FORMAT_DESCRIPTION_EVENT && h.EventType != replication.ROTATE_EVENT {
			continue
		}
		evs = append(evs, e)
	}
	return &blockStream{memStream: memStream{evs: evs}, block: s.f.block}, nil
}

func (s *fakeSession) Close() error {
	s.f.mu.Lock()
	s.f.closed++
	s.f.mu.Unlock()
	return nil
}

// blockStream 读完内存里的事件后，如果设置了 block 就继续阻塞（模拟还在等待新事件的长任务）。
type blockStream struct {
	memStream
	block chan struct{}
}

func (b *blockStream) Next(ctx context.Context) (*replication.BinlogEvent, error) {
	ev, err := b.memStream.Next(ctx)
	if errors.Is(err, io.EOF) && b.block != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-b.block:
			return nil, io.EOF
		}
	}
	return ev, err
}

// ---- 辅助 ----

var (
	root  = audit.Actor{ID: 1, Username: "root", Display: "管理员", IsSuperuser: true}
	alice = audit.Actor{ID: 2, Username: "alice", Display: "爱丽丝"}
	carol = audit.Actor{ID: 3, Username: "carol", Display: "卡罗"}
)

func newSvc(t testing.TB, src *fakeSource) (*Service, *fakeAudit, *fakeRepo) {
	t.Helper()
	repo := newFakeRepo()
	au := &fakeAudit{}
	svc := NewService(repo, src, au)
	svc.spillDir = t.TempDir() // 大结果的临时文件放进测试目录；任务结果要在目录清理之前关掉（Windows 下不能删还打开着的文件）
	t.Cleanup(func() {
		svc.SetRoot(context.Background())
		svc.jobs.mu.Lock()
		defer svc.jobs.mu.Unlock()
		for _, j := range svc.jobs.jobs {
			j.closeResult()
		}
	})
	return svc, au, repo
}

// waitDone 等任务结束并返回最终视图。
func waitDone(t testing.TB, svc *Service, actor audit.Actor, id string) *JobView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		v, err := svc.Job(actor, id)
		if err != nil {
			t.Fatal(err)
		}
		if v.Status != StatusQueued && v.Status != StatusRunning {
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("等待任务结束超时")
	return nil
}

func waitRunning(t testing.TB, src *fakeSource, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		src.mu.Lock()
		got := src.streams
		src.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("等待任务开始拉取超时")
}
