// @author: kevinkelly
// @license: Apache Licence
// @file: fakes_test.go
// @time: 2026/10/28

package giconfig

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/model"
)

// ---- 内存仓库 ----

type memRepo struct {
	mu       sync.Mutex
	noTables bool
	cfg      map[string]string
	versions []*Version
	deploy   *Deploy
	state    map[string]string
	lock     *struct {
		token, owner string
		exp          time.Time
	}
	audits   []model.AuditEntry
	failNext string // "insert"：下次 InsertVersion 失败
	nextID   uint
}

func newMemRepo() *memRepo {
	return &memRepo{cfg: map[string]string{}, state: map[string]string{}, nextID: 1}
}

func (r *memRepo) TableReady() bool { return !r.noTables }
func (r *memRepo) ConfigValue(k string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.cfg[k]
	if !ok || strings.TrimSpace(v) == "" {
		return "", false
	}
	return v, true
}
func (r *memRepo) SetConfigValues(items map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range items {
		r.cfg[k] = v
	}
	return nil
}
func (r *memRepo) copyOf(v *Version, content bool) *Version {
	if v == nil {
		return nil
	}
	c := *v
	if !content {
		c.Content = ""
	}
	return &c
}
func (r *memRepo) LatestVersion() (*Version, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.versions) == 0 {
		return nil, nil
	}
	return r.copyOf(r.versions[len(r.versions)-1], true), nil
}
func (r *memRepo) LatestByStatus(st string) (*Version, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.versions) - 1; i >= 0; i-- {
		if r.versions[i].Status == st {
			return r.copyOf(r.versions[i], true), nil
		}
	}
	return nil, nil
}
func (r *memRepo) GetVersion(n int) (*Version, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range r.versions {
		if v.Version == n {
			return r.copyOf(v, true), nil
		}
	}
	return nil, nil
}
func (r *memRepo) ListVersions(limit int) ([]Version, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Version
	for i := len(r.versions) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, *r.copyOf(r.versions[i], false))
	}
	return out, nil
}
func (r *memRepo) InsertVersion(v *Version) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failNext == "insert" {
		r.failNext = ""
		return errors.New("db down")
	}
	c := *v
	c.ID = r.nextID
	r.nextID++
	c.Version = len(r.versions) + 1
	r.versions = append(r.versions, &c)
	v.ID, v.Version = c.ID, c.Version
	return nil
}
func (r *memRepo) UpdateVersionResult(id uint, from []string, status string, at *time.Time, msg string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range r.versions {
		if v.ID != id {
			continue
		}
		for _, f := range from {
			if v.Status == f {
				v.Status, v.Error = status, msg
				if at != nil {
					v.AppliedAt = at
				}
				return true, nil
			}
		}
	}
	return false, nil
}
func (r *memRepo) PruneVersions(keep int) error { return nil }
func (r *memRepo) LoadDeploy() (Deploy, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.noTables {
		return Deploy{}, false, errors.New("Table 'dbt_gi_setting' doesn't exist")
	}
	if r.deploy == nil {
		return DefaultDeploy(), false, nil
	}
	return *r.deploy, true, nil
}
func (r *memRepo) SaveDeploy(d Deploy, by string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deploy = &d
	return nil
}
func (r *memRepo) LoadState(k string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state[k], nil
}
func (r *memRepo) SaveState(k, v string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state[k] = v
	return nil
}
func (r *memRepo) AcquireLock(owner string, ttl time.Duration) (string, bool, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lock != nil && time.Now().Before(r.lock.exp) {
		return "", false, r.lock.owner, nil
	}
	tok := fmt.Sprintf("tok-%d", time.Now().UnixNano())
	r.lock = &struct {
		token, owner string
		exp          time.Time
	}{tok, owner, time.Now().Add(ttl)}
	return tok, true, "", nil
}
func (r *memRepo) ReleaseLock(tok string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lock != nil && r.lock.token == tok {
		r.lock = nil
	}
	return nil
}
func (r *memRepo) InsertAudit(e *model.AuditEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.audits = append(r.audits, *e)
	return nil
}
func (r *memRepo) auditText() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, a := range r.audits {
		b.WriteString(a.Action + " " + a.ExtraInfo + "\n")
	}
	return b.String()
}
func (r *memRepo) version(n int) *Version {
	v, _ := r.GetVersion(n)
	return v
}

// ---- 内存文件 ----

type memFiles struct {
	mu        sync.Mutex
	files     map[string]string
	writes    []string // 写入历史（路径）
	failWrite map[int]error
	nWrites   int
	checkErr  error
}

func newMemFiles() *memFiles {
	return &memFiles{files: map[string]string{}, failWrite: map[int]error{}}
}

func (f *memFiles) Read(p string) ([]byte, FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.files[p]
	if !ok {
		return nil, FileInfo{}, nil
	}
	return []byte(c), FileInfo{Exists: true, Size: int64(len(c)), ModTime: time.Unix(1700000000, 0), Mode: 0o640}, nil
}
func (f *memFiles) Write(p string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nWrites++
	if err := f.failWrite[f.nWrites]; err != nil {
		return err
	}
	f.files[p] = string(data)
	f.writes = append(f.writes, p)
	return nil
}
func (f *memFiles) CheckWritable(string) error { return f.checkErr }
func (f *memFiles) get(p string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.files[p]
}
func (f *memFiles) set(p, c string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[p] = c
}

// ---- 假探测与假重启 ----

type fakeProber struct {
	mu        sync.Mutex
	up        bool
	pings     int
	pingErr   error // 非空时覆盖 up
	version   string
	tasks     Tasks
	tasksFn   func(call int) Tasks
	tasksCall int
	backup    BackupResult
	backupErr error
	lastConn  ConnInfo
}

func (p *fakeProber) Ping(ctx context.Context, c ConnInfo) (PingResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pings++
	p.lastConn = c
	if p.pingErr != nil {
		return PingResult{}, p.pingErr
	}
	if !p.up {
		return PingResult{}, errors.New("connection refused")
	}
	return PingResult{LatencyMS: 7, Version: p.version, Variables: 12}, nil
}
func (p *fakeProber) Tasks(ctx context.Context, c ConnInfo) (Tasks, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tasksCall++
	if p.tasksFn != nil {
		return p.tasksFn(p.tasksCall), nil
	}
	return p.tasks, nil
}
func (p *fakeProber) PingBackup(ctx context.Context, c ConnInfo) (BackupResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastConn = c
	return p.backup, p.backupErr
}
func (p *fakeProber) setUp(v bool) {
	p.mu.Lock()
	p.up = v
	p.mu.Unlock()
}

// fakeDocker 是假的 CommandRunner：只记录被执行的 argv，不真的执行任何命令。
// 它模拟 docker restart 的效果：读磁盘上的配置，含 BAD_PARAM 就起不来（goInception 不再可连接）。
type fakeDocker struct {
	mu      sync.Mutex
	calls   int
	argvs   [][]string // 每次被执行的 argv
	files   *memFiles
	prober  *fakeProber
	path    string
	err     error         // 重启命令本身失败
	failOn  map[int]error // 第 n 次调用失败
	block   chan struct{} // 非空时 Run 会阻塞到它被关闭
	started chan struct{} // 非空时进入 Run 会先通知
}

func (r *fakeDocker) Run(ctx context.Context, argv []string) (string, error) {
	r.mu.Lock()
	r.calls++
	n := r.calls
	r.argvs = append(r.argvs, append([]string(nil), argv...))
	started, block := r.started, r.block
	r.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if err := r.failOn[n]; err != nil {
		return "docker failed", err
	}
	if r.err != nil {
		return "docker failed", r.err
	}
	r.prober.setUp(!strings.Contains(r.files.get(r.path), "BAD_PARAM"))
	return "restarted", nil
}
func (r *fakeDocker) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// commands 返回每次被执行的命令（空格拼接）。
func (r *fakeDocker) commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.argvs))
	for i, a := range r.argvs {
		out[i] = strings.Join(a, " ")
	}
	return out
}

// ---- 测试环境 ----

const testPath = "/etc/goinception/config.toml"

type env struct {
	t     *testing.T
	repo  *memRepo
	files *memFiles
	pr    *fakeProber
	rs    *fakeDocker
	svc   *Service
	clock *fakeClock
	actor audit.Actor
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
	return ctx.Err()
}

func newEnv(t *testing.T) *env {
	t.Helper()
	repo, files := newMemRepo(), newMemFiles()
	pr := &fakeProber{up: true, version: "v1.2.7", tasks: Tasks{Known: true}}
	rs := &fakeDocker{files: files, prober: pr, path: testPath, failOn: map[int]error{}}
	clk := &fakeClock{t: time.Date(2026, 10, 6, 12, 0, 0, 0, time.Local)}
	svc := NewService(repo, Options{Files: files, Prober: pr, Runner: rs, Env: func(string) string { return "" },
		Now: clk.now, Sleep: clk.sleep, PollInterval: time.Second})
	repo.cfg[cfgFile] = testPath
	repo.cfg[cfgHost], repo.cfg[cfgPort], repo.cfg[cfgUser], repo.cfg[cfgPass] = "10.20.9.30", "4000", "inception", "pw-main"
	return &env{t: t, repo: repo, files: files, pr: pr, rs: rs, svc: svc, clock: clk, actor: audit.Actor{ID: 1, Username: "root", Display: "管理员", IsSuperuser: true}}
}

// testContainer 是测试里用的容器名。
const testContainer = "goinception"

// useDocker 把容器名设为 testContainer（重启命令为 docker restart goinception）。
func (e *env) useDocker() {
	e.t.Helper()
	d := DefaultDeploy()
	d.Container = testContainer
	if err := e.repo.SaveDeploy(d, "root"); err != nil {
		e.t.Fatal(err)
	}
}

const baseConf = `# goInception
[inc]
lang = "zh-CN"
check_primary_key = true
check_dml_limit = false
max_update_rows = 5000
backup_host = "10.20.9.31"
backup_password = "s3cret"

[log]
level = "info"
`

// seed 写入磁盘文件，并导入为已生效的版本 #1。
func (e *env) seed() {
	e.t.Helper()
	e.files.set(testPath, baseConf)
	if _, err := e.svc.ImportDisk(e.actor, ImportRequest{AsApplied: true}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) overview() *Overview {
	e.t.Helper()
	o, err := e.svc.Overview()
	if err != nil {
		e.t.Fatal(err)
	}
	return o
}

// mustErr 断言错误是 *Error 且状态码/错误码匹配。
func mustErr(t *testing.T, err error, status int, code string) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("期望 *Error(%d %s)，得到 %v", status, code, err)
	}
	if e.Status != status || e.Code != code {
		t.Fatalf("期望 %d %s，得到 %d %s：%s", status, code, e.Status, e.Code, e.Message)
	}
	return e
}
