// @author: kevinkelly
// @license: Apache Licence
// @file: memstore_test.go
// @time: 2026/10/28

package dbtadmin

import (
	"bytes"
	"context"
	"dbtool/internal/settings"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// memStore 是 Store 的内存假实现：记录执行过的语句，按 CREATE TABLE 维护“已有的表”，用户 / 审计放在内存里，事务靠快照回滚。
// 表名与真实库一致：dbt_ 前缀（dbt_sql_users、dbt_audit_log …）。
type memStore struct {
	tables  map[string]bool
	counts  map[string]int64 // 非用户表的行数（dbt_auth_permission 等）
	execs   []string
	failOn  string // 语句包含该子串时返回 failErrMsg
	failMsg string
	version string
	db      string

	charset    string // 库默认字符集 / 校验规则（newMemStore 默认 utf8mb4 / utf8mb4_general_ci）
	collation  string
	charsetErr error

	existingErr error // ExistingTables 返回的错误（模拟读不到 information_schema）

	applied   map[string]string
	appliedAt map[string]time.Time

	users     []*User
	hashes    map[uint]string
	userGroup map[uint][]uint
	groups    map[string]uint
	profile   map[uint]AuthInfo
	settings  map[string]string
	configs   map[string]string

	settingRows map[string]settings.Row // dbt_setting（SettingRows / ApplySettings）；nil = 表不存在
	settingHist []settings.History

	audits []AuditEntry
	nextID uint

	perm    PermMigrator // dbt-admin perm migrate 用的迁移入口（permcmd_test.go 里的假实现）
	permErr error

	secrets   []SecretRow
	secretErr error

	cells     []Cell // EncryptedCells 返回的加密列值；ReplaceCell 会改它
	cellsErr  error
	failOnSet int // ReplaceCell 第 N 次调用返回错误（0 = 不注入）；用来测整体回滚
	staleAt   int // ReplaceCell 第 N 次调用当作“值已被别人改过”：不改并返回 false
	setCalls  int

	closed   bool
	txCount  int
	dupOnIns bool // InsertUser 返回 1062 冲突（模拟并发创建）
}

func newMemStore() *memStore {
	return &memStore{
		tables:    map[string]bool{},
		counts:    map[string]int64{},
		applied:   map[string]string{},
		appliedAt: map[string]time.Time{},
		hashes:    map[uint]string{},
		userGroup: map[uint][]uint{},
		groups:    map[string]uint{},
		profile:   map[uint]AuthInfo{},
		settings:  map[string]string{},
		configs:   map[string]string{},
		version:   "8.0.36",
		db:        "db_tool",
		charset:   "utf8mb4",
		collation: "utf8mb4_general_ci",
		nextID:    1,
	}
}

var reCreate = regexp.MustCompile(`(?i)^CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?` + "`?" + `([A-Za-z0-9_]+)`)
var reInsertPerm = regexp.MustCompile(`(?i)^INSERT\s+(?:IGNORE\s+)?INTO\s+` + "`?" + `dbt_auth_permission`)

func (m *memStore) Close() error { m.closed = true; return nil }
func (m *memStore) ServerInfo(context.Context) (ServerInfo, error) {
	return ServerInfo{Version: m.version, Database: m.db}, nil
}
func (m *memStore) HasTable(_ context.Context, n string) (bool, error) { return m.tables[n], nil }
func (m *memStore) CountRows(_ context.Context, t string) (int64, error) {
	if t == tblUsers {
		return int64(len(m.users)), nil
	}
	return m.counts[t], nil
}
func (m *memStore) CountBaseTables(context.Context) (int, error) { return len(m.tables), nil }
func (m *memStore) ExistingTables(_ context.Context, names []string) ([]string, error) {
	var out []string
	for _, n := range names {
		if m.tables[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, m.existingErr
}
func (m *memStore) DatabaseCharset(context.Context) (DBCharset, error) {
	return DBCharset{Charset: m.charset, Collation: m.collation}, m.charsetErr
}

func (m *memStore) ExecStmt(_ context.Context, stmt string) error {
	if m.failOn != "" && strings.Contains(stmt, m.failOn) {
		return errors.New(m.failMsg)
	}
	m.execs = append(m.execs, stmt)
	if sub := reCreate.FindStringSubmatch(stmt); sub != nil {
		m.tables[sub[1]] = true
	}
	if reInsertPerm.MatchString(stmt) {
		m.counts[tblPermission]++
	}
	return nil
}

func (m *memStore) EnsureAppliedTable(context.Context) error { m.tables[TblApplied] = true; return nil }
func (m *memStore) AppliedSHAs(context.Context) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range m.applied {
		out[k] = v
	}
	return out, nil
}
func (m *memStore) RecordApplied(_ context.Context, f, sha, ver string, at time.Time) error {
	m.applied[f] = sha
	m.appliedAt[f] = at
	return nil
}
func (m *memStore) AuthSetting(_ context.Context, k string) (string, bool, error) {
	v, ok := m.settings[k]
	return v, ok, nil
}
func (m *memStore) ConfigValue(_ context.Context, k string) (string, bool, error) {
	v, ok := m.configs[k]
	return v, ok, nil
}
func (m *memStore) ListUsers(context.Context) ([]User, error) {
	var out []User
	for _, u := range m.users {
		out = append(out, *u)
	}
	return out, nil
}

// addUser 直接放一个用户（测试准备数据用）。
func (m *memStore) addUser(name string, super, active bool) *User {
	m.tables[tblUsers] = true
	u := &User{ID: m.nextID, Username: name, Display: name, IsSuperuser: super, IsStaff: super, IsActive: active}
	m.nextID++
	m.users = append(m.users, u)
	m.hashes[u.ID] = "pbkdf2_sha256$1$old$old"
	return u
}

func (m *memStore) user(name string) *User {
	for _, u := range m.users {
		if u.Username == name {
			return u
		}
	}
	return nil
}

type memSnap struct {
	cells     []Cell
	users     []User
	hashes    map[uint]string
	userGroup map[uint][]uint
	profile   map[uint]AuthInfo
	audits    []AuditEntry
	nextID    uint
}

func (m *memStore) snap() memSnap {
	s := memSnap{hashes: map[uint]string{}, userGroup: map[uint][]uint{}, profile: map[uint]AuthInfo{}, nextID: m.nextID}
	for _, u := range m.users {
		s.users = append(s.users, *u)
	}
	for k, v := range m.hashes {
		s.hashes[k] = v
	}
	for k, v := range m.userGroup {
		s.userGroup[k] = append([]uint(nil), v...)
	}
	for k, v := range m.profile {
		s.profile[k] = v
	}
	s.audits = append([]AuditEntry(nil), m.audits...)
	s.cells = append([]Cell(nil), m.cells...)
	return s
}

func (m *memStore) restore(s memSnap) {
	m.users = nil
	for i := range s.users {
		u := s.users[i]
		m.users = append(m.users, &u)
	}
	m.hashes, m.userGroup, m.profile, m.audits, m.nextID = s.hashes, s.userGroup, s.profile, s.audits, s.nextID
	m.cells = s.cells
}

func (m *memStore) InTx(_ context.Context, fn func(Tx) error) error {
	m.txCount++
	s := m.snap()
	if err := fn(&memTx{m: m}); err != nil {
		m.restore(s)
		return err
	}
	return nil
}

type memTx struct{ m *memStore }

func (t *memTx) GetUser(name string) (*User, error) {
	if u := t.m.user(name); u != nil {
		c := *u
		return &c, nil
	}
	return nil, nil
}
func (t *memTx) CountOtherActiveSuperusers(ex uint) (int, error) {
	n := 0
	for _, u := range t.m.users {
		if u.IsSuperuser && u.IsActive && u.ID != ex {
			n++
		}
	}
	return n, nil
}
func (t *memTx) GroupIDs(names []string) (map[string]uint, error) {
	out := map[string]uint{}
	for _, n := range names {
		if id, ok := t.m.groups[n]; ok {
			out[n] = id
		}
	}
	return out, nil
}
func (t *memTx) InsertUser(u NewUser) (uint, error) {
	if t.m.dupOnIns {
		return 0, errors.New("Error 1062 (23000): Duplicate entry")
	}
	if t.m.user(u.Username) != nil {
		return 0, errors.New("Error 1062 (23000): Duplicate entry")
	}
	nu := &User{ID: t.m.nextID, Username: u.Username, Display: u.Display, Email: u.Email, IsSuperuser: u.IsSuperuser,
		IsStaff: u.IsStaff, IsActive: u.IsActive}
	t.m.nextID++
	t.m.users = append(t.m.users, nu)
	t.m.hashes[nu.ID] = u.PasswordHash
	return nu.ID, nil
}
func (t *memTx) AddUserGroups(id uint, gs []uint) error {
	t.m.userGroup[id] = append(t.m.userGroup[id], gs...)
	return nil
}
func (t *memTx) find(id uint) *User {
	for _, u := range t.m.users {
		if u.ID == id {
			return u
		}
	}
	return nil
}
func (t *memTx) SetPassword(id uint, h string) error {
	t.m.hashes[id] = h
	return t.ClearFailedLogins(id)
}
func (t *memTx) ClearFailedLogins(id uint) error {
	if u := t.find(id); u != nil {
		u.FailedLoginCount, u.LastFailedAt = 0, nil
	}
	return nil
}
func (t *memTx) SetActive(id uint, a bool) error {
	t.find(id).IsActive = a
	return nil
}
func (t *memTx) AuthInfo(id uint) (AuthInfo, error) { return t.m.profile[id], nil }
func (t *memTx) SetMustChange(id uint, v bool, by string, at time.Time) error {
	if !t.m.tables[tblUserProfile] {
		return ErrTableMissing
	}
	p := t.m.profile[id]
	p.HasProfile, p.MustChange = true, v
	t.m.profile[id] = p
	return nil
}
func (t *memTx) InsertAudit(e AuditEntry) error {
	if !t.m.tables[tblAuditLog] {
		return ErrTableMissing
	}
	t.m.audits = append(t.m.audits, e)
	return nil
}

// ---- 测试环境 ----

type testEnv struct {
	*Env
	out, errb *bytes.Buffer
	st        *memStore
	env       map[string]string
	tty       bool
	pwQueue   []string // ReadPassword 依次返回
	pwPrompts []string
	stdin     *bytes.Buffer
}

const testDBURL = "mysql://dbtool:Pw%40S3cretDb@10.9.8.7:3306/db_tool"

func newTestEnv(t *testing.T, st *memStore) *testEnv {
	t.Helper()
	te := &testEnv{out: &bytes.Buffer{}, errb: &bytes.Buffer{}, st: st, env: map[string]string{"DATABASE_URL": testDBURL}, stdin: &bytes.Buffer{}}
	te.Env = &Env{
		Getenv: func(k string) string { return te.env[k] },
		Stdin:  te.stdin,
		Stdout: te.out, Stderr: te.errb,
		IsTTY: func() bool { return te.tty },
		ReadPassword: func(p string) (string, error) {
			te.pwPrompts = append(te.pwPrompts, p)
			if len(te.pwQueue) == 0 {
				return "", errors.New("no more passwords")
			}
			v := te.pwQueue[0]
			te.pwQueue = te.pwQueue[1:]
			return v, nil
		},
		Open: func(_ context.Context, p ConnParams) (Store, error) {
			if st == nil {
				return nil, errors.New("no store")
			}
			return st, nil
		},
		Now:        func() time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC) },
		OSUser:     func() string { return "alice" },
		Version:    "test",
		Iterations: 1000,
	}
	return te
}

func (te *testEnv) run(args ...string) int {
	return Main(context.Background(), args, te.Env)
}

// all 返回 stdout + stderr 的全部输出。
func (te *testEnv) all() string { return te.out.String() + te.errb.String() }

func (te *testEnv) reset() { te.out.Reset(); te.errb.Reset() }

// writeFiles 在 root 下按 map（相对路径 → 内容）写文件。
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func mustNotContain(t *testing.T, label, s string, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if b != "" && strings.Contains(s, b) {
			t.Errorf("%s 里不应出现 %q：\n%s", label, b, s)
		}
	}
}

func mustContain(t *testing.T, label, s string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(s, w) {
			t.Errorf("%s 里应包含 %q：\n%s", label, w, s)
		}
	}
}

var _ = fmt.Sprintf

func (m *memStore) SecretRows(context.Context) ([]SecretRow, error) {
	if m.secretErr != nil {
		return nil, m.secretErr
	}
	return m.secrets, nil
}
