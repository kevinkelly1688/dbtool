// @author: kevinkelly
// @license: Apache Licence
// @file: fake_test.go
// @time: 2026/10/28

package authx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"dbtool/internal/crypto"
	"dbtool/internal/model"
)

// memRepo 是 Repo 的内存假实现（语义对齐 store.go）。
type memRepo struct {
	mu        sync.Mutex
	users     map[uint]*model.User
	nextUID   uint
	providers map[uint]*Provider
	nextPID   uint
	idents    map[string]uint // providerID|subject → uid
	cfg       map[string]string
	kv        map[string]string
	tfa       map[uint][]TwoFAConfig
	authGrp   []IDName
	resGrp    []IDName
	userAuth  map[uint]map[uint]bool
	userRes   map[uint]map[uint]bool
	audits    []model.AuditEntry
	failOn    string // 非空时对应方法返回错误（测试故障路径）

	dept       map[uint]string // 假的 dbt_user_profile
	mustChange map[uint]bool
}

func newMemRepo() *memRepo {
	return &memRepo{users: map[uint]*model.User{}, nextUID: 100, providers: map[uint]*Provider{}, nextPID: 0,
		idents: map[string]uint{}, cfg: map[string]string{}, kv: map[string]string{}, tfa: map[uint][]TwoFAConfig{},
		userAuth: map[uint]map[uint]bool{}, userRes: map[uint]map[uint]bool{}, dept: map[uint]string{}, mustChange: map[uint]bool{},
		authGrp: []IDName{{1, "Default"}, {2, "RD"}, {3, "DBA"}}, resGrp: []IDName{{1, "研发组"}, {2, "数据组"}}}
}

func (r *memRepo) addUser(id uint, name, password string, super bool) *model.User {
	h := ""
	if password != "" {
		h, _ = crypto.MakeDjangoPassword(password, "salty", 1000)
	} else {
		h = "!unusable"
	}
	u := &model.User{ID: id, Username: name, Password: h, IsActive: true, IsSuperuser: super, Display: name}
	r.users[id] = u
	return u
}

func (r *memRepo) UserByID(id uint) (*model.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if u, ok := r.users[id]; ok {
		c := *u
		return &c, nil
	}
	return nil, nil
}

func (r *memRepo) UserByName(name string) (*model.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, u := range r.users {
		if strings.EqualFold(u.Username, name) {
			c := *u
			return &c, nil
		}
	}
	return nil, nil
}

func (r *memRepo) UsersByEmail(email string) ([]model.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []model.User
	for _, u := range r.users {
		if u.Email != "" && strings.EqualFold(u.Email, email) {
			out = append(out, *u)
		}
	}
	return out, nil
}

func (r *memRepo) UserByThirdID(kind, id string) (*model.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, u := range r.users {
		v := map[string]string{"ding": u.DingUserID, "feishu": u.FeishuOpenID, "slack": u.SlackID}[kind]
		if v != "" && v == id {
			c := *u
			return &c, nil
		}
	}
	return nil, nil
}

func (r *memRepo) CreateUser(u *model.User) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.users {
		if strings.EqualFold(e.Username, u.Username) {
			return errDup
		}
	}
	r.nextUID++
	u.ID = r.nextUID
	c := *u
	r.users[u.ID] = &c
	return nil
}

type dupErr struct{}

func (dupErr) Error() string { return "duplicate" }

var errDup = dupErr{}

func (r *memRepo) RecordLoginFailure(uid uint, at time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u := r.users[uid]
	u.FailedLoginCount = min(127, u.FailedLoginCount+1)
	u.LastLoginFailedAt = &at
	return u.FailedLoginCount, nil
}

func (r *memRepo) ResetLoginFailures(uid uint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users[uid].FailedLoginCount = 0
	return nil
}

func (r *memRepo) MarkLogin(uid uint, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	u := r.users[uid]
	u.LastLogin, u.FailedLoginCount = &at, 0
	return nil
}

func (r *memRepo) UpdateProfile(uid uint, display, email string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users[uid].Display, r.users[uid].Email = display, email
	return nil
}

func (r *memRepo) SetPassword(uid uint, hash string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users[uid].Password = hash
	return nil
}

func (r *memRepo) UsersByPhone(phone string) ([]model.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []model.User
	for uid, cs := range r.tfa {
		for _, c := range cs {
			if c.Type == "sms" && c.Phone == phone && r.users[uid] != nil && r.users[uid].IsActive {
				out = append(out, *r.users[uid])
			}
		}
	}
	return out, nil
}

func (r *memRepo) IdentitiesByUser(uid uint) ([]BoundIdentity, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []BoundIdentity
	for k, u := range r.idents {
		if u != uid {
			continue
		}
		pid, _ := strconv.ParseUint(strings.SplitN(k, "|", 2)[0], 10, 32)
		out = append(out, BoundIdentity{ID: r.identID(k), ProviderID: uint(pid)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// identID 给绑定记录一个稳定的编号（测试里按键名排序后的位置 + 1000）。
func (r *memRepo) identID(key string) uint {
	keys := make([]string, 0, len(r.idents))
	for k := range r.idents {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		if k == key {
			return uint(1000 + i)
		}
	}
	return 0
}

func (r *memRepo) DeleteIdentityByID(id, uid uint) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, u := range r.idents {
		if u == uid && r.identID(k) == id {
			delete(r.idents, k)
			return true, nil
		}
	}
	return false, nil
}

func (r *memRepo) CountTwoFA() (int, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	total, bound := 0, 0
	for uid, u := range r.users {
		if u.IsActive {
			total++
			if len(r.tfa[uid]) > 0 {
				bound++
			}
		}
	}
	return bound, total, nil
}

// ---- ProfileRepo（旁路表 dbt_user_profile 的假实现）----

func (r *memRepo) UserDept(uid uint) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dept[uid]
}

func (r *memRepo) MustChangePassword(uid uint) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mustChange[uid]
}

func (r *memRepo) ClearMustChange(uid uint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.mustChange, uid)
	return nil
}

func (r *memRepo) SetThirdID(uid uint, kind, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	u := r.users[uid]
	switch kind {
	case "ding":
		u.DingUserID = id
	case "feishu":
		u.FeishuOpenID = id
	case "slack":
		u.SlackID = id
	}
	return nil
}

func (r *memRepo) ListUsers(limit int) ([]IDName, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []IDName
	for _, u := range r.users {
		if u.IsActive {
			out = append(out, IDName{u.ID, u.Username})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (r *memRepo) ListAuthGroups() ([]IDName, error)     { return r.authGrp, nil }
func (r *memRepo) ListResourceGroups() ([]IDName, error) { return r.resGrp, nil }
func idsByName(list []IDName, names []string) []uint {
	var out []uint
	for _, g := range list {
		for _, n := range names {
			if g.Name == n {
				out = append(out, g.ID)
			}
		}
	}
	return out
}
func (r *memRepo) AuthGroupIDsByName(n []string) ([]uint, error) { return idsByName(r.authGrp, n), nil }
func (r *memRepo) ResourceGroupIDsByName(n []string) ([]uint, error) {
	return idsByName(r.resGrp, n), nil
}

func (r *memRepo) AddUserAuthGroups(uid uint, ids []uint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.userAuth[uid] == nil {
		r.userAuth[uid] = map[uint]bool{}
	}
	for _, id := range ids {
		r.userAuth[uid][id] = true
	}
	return nil
}

func (r *memRepo) AddUserResourceGroups(uid uint, ids []uint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.userRes[uid] == nil {
		r.userRes[uid] = map[uint]bool{}
	}
	for _, id := range ids {
		r.userRes[uid][id] = true
	}
	return nil
}

func (r *memRepo) ListProviders() ([]Provider, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Provider
	for _, p := range r.providers {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sort != out[j].Sort {
			return out[i].Sort < out[j].Sort
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (r *memRepo) GetProvider(id uint) (*Provider, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.providers[id]; ok {
		c := *p
		return &c, nil
	}
	return nil, nil
}

func (r *memRepo) SaveProvider(p *Provider) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.ID == 0 {
		r.nextPID++
		p.ID = r.nextPID
	}
	c := *p
	if old, ok := r.providers[p.ID]; ok {
		c.LastLogin, c.CreateTime = old.LastLogin, old.CreateTime
	}
	r.providers[p.ID] = &c
	return nil
}

func (r *memRepo) DeleteProvider(id uint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.providers, id)
	for k := range r.idents {
		if strings.HasPrefix(k, itoa(id)+"|") {
			delete(r.idents, k)
		}
	}
	return nil
}

func itoa(id uint) string { return strconv.FormatUint(uint64(id), 10) }

func (r *memRepo) SetProviderTest(id uint, ok bool, msg, hash string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.providers[id]
	p.TestOK, p.TestMsg, p.TestedHash, p.TestTime = &ok, msg, hash, &at
	return nil
}

func (r *memRepo) SetProviderEnabled(id uint, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[id].Enabled = enabled
	return nil
}

func (r *memRepo) SetProviderOrder(ids []uint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, id := range ids {
		r.providers[id].Sort = (i + 1) * 10
	}
	return nil
}

func (r *memRepo) TouchProviderLogin(id uint, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[id].LastLogin = &at
	return nil
}

func identKey(pid uint, subject string) string { return itoa(pid) + "|" + subject }

func (r *memRepo) IdentityUser(pid uint, subject string) (uint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.idents[identKey(pid, subject)], nil
}

func (r *memRepo) BindIdentity(pid uint, subject string, uid uint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.idents[identKey(pid, subject)] = uid
	return nil
}

func (r *memRepo) DeleteIdentity(pid uint, subject string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.idents, identKey(pid, subject))
	return nil
}

func (r *memRepo) CountIdentities(pid uint) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for k := range r.idents {
		if strings.HasPrefix(k, itoa(pid)+"|") {
			n++
		}
	}
	return n, nil
}

func (r *memRepo) ConfigValues(items []string) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]string{}
	for _, i := range items {
		if v, ok := r.cfg[i]; ok {
			out[i] = v
		}
	}
	return out, nil
}

func (r *memRepo) SetConfigValues(kv map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range kv {
		r.cfg[k] = v
	}
	return nil
}

func (r *memRepo) AuthSettings() (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]string{}
	for k, v := range r.kv {
		out[k] = v
	}
	return out, nil
}

func (r *memRepo) SetAuthSettings(kv map[string]string, by string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range kv {
		r.kv[k] = v
	}
	return nil
}

func (r *memRepo) TwoFAConfigs(uid uint) ([]TwoFAConfig, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]TwoFAConfig(nil), r.tfa[uid]...), nil
}

func (r *memRepo) SaveTwoFA(uid uint, username string, c TwoFAConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var keep []TwoFAConfig
	for _, e := range r.tfa[uid] {
		if e.Type != c.Type {
			keep = append(keep, e)
		}
	}
	r.tfa[uid] = append(keep, c)
	return nil
}

func (r *memRepo) DeleteTwoFA(uid uint, typ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var keep []TwoFAConfig
	for _, e := range r.tfa[uid] {
		if e.Type != typ {
			keep = append(keep, e)
		}
	}
	r.tfa[uid] = keep
	return nil
}

func (r *memRepo) InsertAudit(e *model.AuditEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.audits = append(r.audits, *e)
	return nil
}

func (r *memRepo) auditActions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, a := range r.audits {
		out = append(out, a.Action)
	}
	return out
}

// ---- 测试辅助 ----

// fakeClock 是可调的时钟。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 6, 9, 0, 0, 0, time.Local)} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeDir 是假的目录服务。
type fakeDir struct {
	users   map[string]fakeDirUser // 登录名 → 用户
	pingErr error
	down    bool
	calls   int
}

type fakeDirUser struct {
	password string
	dn       string
	attrs    map[string][]string
}

func (d *fakeDir) Authenticate(_ context.Context, _ *Config, username, password string, _ []string) (*DirEntry, error) {
	d.calls++
	if d.down {
		return nil, ErrUnavailable
	}
	u, ok := d.users[username]
	if !ok || u.password != password || password == "" {
		return nil, ErrDirBadCredentials
	}
	return &DirEntry{DN: u.dn, Attrs: u.attrs}, nil
}

func (d *fakeDir) Ping(context.Context, *Config) (string, error) {
	if d.pingErr != nil {
		return "", d.pingErr
	}
	return "连接成功", nil
}

type testEnv struct {
	t     *testing.T
	repo  *memRepo
	svc   *Service
	clk   *fakeClock
	dir   *fakeDir
	sms   *fakeSMS
	admin *model.User
}

// newTestEnv 构造服务：默认带 root(超级用户)/bob/off(停用) 三个本地用户，密码均为 pw-123456789。
func newTestEnv(t *testing.T) *testEnv { return newTestEnvHTTP(t, nil) }

func newTestEnvHTTP(t *testing.T, hc *http.Client) *testEnv {
	t.Helper()
	repo := newMemRepo()
	repo.addUser(1, "root", "pw-123456789", true)
	repo.addUser(2, "bob", "pw-123456789", false)
	off := repo.addUser(3, "off", "pw-123456789", false)
	off.IsActive = false
	clk := newClock()
	dir := &fakeDir{users: map[string]fakeDirUser{}}
	sms := &fakeSMS{}
	svc, err := NewService(repo, Options{Key: []byte("unit-test-secret-key-unit-test-secret-key"), PublicURL: "http://dba.test",
		Now: clk.Now, Dir: dir, SMS: sms, SettingsTTL: time.Millisecond, HTTP: hc})
	if err != nil {
		t.Fatal(err)
	}
	return &testEnv{t: t, repo: repo, svc: svc, clk: clk, dir: dir, sms: sms}
}

// rewriteTransport 把所有请求改投到同一个测试服务器（保留路径与查询串），并把原始主机名放进 X-Orig-Host。
type rewriteTransport struct{ target *url.URL }

func (rt rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c := r.Clone(r.Context())
	c.Header.Set("X-Orig-Host", r.URL.Host)
	c.URL.Scheme, c.URL.Host = rt.target.Scheme, rt.target.Host
	return http.DefaultTransport.RoundTrip(c)
}

func rewriteClient(srv *httptest.Server) *http.Client {
	u, _ := url.Parse(srv.URL)
	return &http.Client{Transport: rewriteTransport{u}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// fakeSMS 记录发出的短信。
type fakeSMS struct {
	mu   sync.Mutex
	sent []struct{ Phone, Code string }
	err  error
	off  bool
}

func (f *fakeSMS) Sender(set SMSSettings) (SMSSender, error) {
	if set.Provider == SMSDisabled || f.off {
		return nil, ErrSMSDisabled
	}
	return f, nil
}

func (f *fakeSMS) Send(_ context.Context, phone, code string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, struct{ Phone, Code string }{phone, code})
	return nil
}

func (f *fakeSMS) last() (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return "", ""
	}
	s := f.sent[len(f.sent)-1]
	return s.Phone, s.Code
}
