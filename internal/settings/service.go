// @author: kevinkelly
// @license: Apache Licence
// @file: service.go
// @time: 2026/10/28

package settings

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"dbtool/internal/model"
)

// Cipher 是敏感设置的加解密（*crypto.Mirage 满足）。
type Cipher interface {
	Encrypt(string) string
	Decrypt(string) string
	Undecryptable(string) bool
}

// PlainCipher 不加密（测试 / 没有配置密钥的演示环境）。
type PlainCipher struct{}

func (PlainCipher) Encrypt(s string) string   { return s }
func (PlainCipher) Decrypt(s string) string   { return s }
func (PlainCipher) Undecryptable(string) bool { return false }

// Actor 是修改设置的人（写变更记录与系统审计用）。
type Actor struct {
	ID       uint
	Username string
	Display  string
}

// Error 是修改设置时的业务错误：Status 是 HTTP 状态码，Code 给页面，Fields 是逐项的校验错误（键 → 说明）。
type Error struct {
	Status  int
	Code    string
	Message string
	Fields  map[string]string
}

func (e *Error) Error() string { return e.Message }

func newErr(status int, code, format string, a ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, a...)}
}

// Change 是一次实际发生的修改。
type Change struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Old    string `json:"old"`
	New    string `json:"new"`
	Secret bool   `json:"secret"`
	Reset  bool   `json:"reset"` // 恢复默认 / 清除
}

// DefaultTTL 是快照缓存的有效期：本进程写入后立即失效，其它进程最多延迟这么久生效。
const DefaultTTL = 3 * time.Second

type entry struct {
	val string // 解密后的值（解不开时为空）
	row Row
	bad bool // 密文用当前密钥解不开
}

type snapshot struct {
	m     map[string]entry
	ready bool
	at    time.Time
}

// Service 是设置服务：读（带缓存）、改（校验 / 加密 / 变更记录 / 审计）。
type Service struct {
	store Store
	ci    Cipher
	ttl   time.Duration
	now   func() time.Time

	mu   sync.Mutex
	snap *snapshot

	warnOnce sync.Once
}

// New 返回设置服务。
func New(store Store, ci Cipher) *Service {
	if ci == nil {
		ci = PlainCipher{}
	}
	return &Service{store: store, ci: ci, ttl: DefaultTTL, now: time.Now}
}

var (
	openMu    sync.Mutex
	openCache = map[*gorm.DB]*Service{}
)

// Open 返回元数据库 db 上的共享服务（同一个 *gorm.DB 只创建一个，各模块的 store 层用它读设置）。
func Open(db *gorm.DB, ci Cipher) *Service {
	openMu.Lock()
	defer openMu.Unlock()
	if s, ok := openCache[db]; ok {
		// 先被没有密钥的调用方创建过（PlainCipher）时，拿到真正的密钥后升级：敏感项必须用它加密
		if ci != nil {
			s.mu.Lock()
			if _, plain := s.ci.(PlainCipher); plain {
				s.ci = ci
				s.snap = nil
			}
			s.mu.Unlock()
		}
		return s
	}
	s := New(NewGormStore(db), ci)
	openCache[db] = s
	return s
}

// NewMemory 返回内存服务（测试 / 演示）：initial 里的值作为“显式设置”，不加密。
func NewMemory(initial map[string]string) *Service {
	ms := NewMemStore()
	for k, v := range initial {
		ms.Put(k, v, false)
	}
	return New(ms, PlainCipher{})
}

// SetTTL 调整缓存有效期（测试用）。
func (s *Service) SetTTL(d time.Duration) { s.ttl = d; s.Invalidate() }

// Invalidate 丢弃快照，下次读取重新加载。模块用 WriteTx 在自己的事务里写完设置后调用。
func (s *Service) Invalidate() {
	s.mu.Lock()
	s.snap = nil
	s.mu.Unlock()
}

// load 返回当前快照（过期则重新加载）。加载失败时沿用旧快照并稍后重试。
func (s *Service) load() *snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snap != nil && s.now().Sub(s.snap.at) < s.ttl {
		return s.snap
	}
	rows, ready, err := s.store.Load()
	if err != nil {
		slog.Error("读取系统设置失败", "err", err)
		if s.snap != nil {
			s.snap.at = s.now()
			return s.snap
		}
		s.snap = &snapshot{m: map[string]entry{}, ready: false, at: s.now()}
		return s.snap
	}
	sn := &snapshot{m: make(map[string]entry, len(rows)), ready: ready, at: s.now()}
	if !ready {
		s.warnOnce.Do(func() {
			slog.Error("系统设置表 dbt_setting 不存在：请先在元数据库执行 deploy/sql/260_setting.sql（install.sh upgrade 会自动执行），在此之前所有设置按默认值运行")
		})
	}
	for k, r := range rows {
		e := entry{row: r}
		if r.Encrypted {
			if s.ci.Undecryptable(r.Value) {
				e.bad = true
			} else {
				e.val = s.ci.Decrypt(r.Value)
			}
		} else {
			e.val = r.Value
		}
		sn.m[k] = e
	}
	s.snap = sn
	return sn
}

// UndecryptableKeys 返回库里“密文用当前密钥解不开”的设置键（只返回键名，升序）。
func (s *Service) UndecryptableKeys() []string {
	var out []string
	for k, e := range s.load().m {
		if e.bad {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Ready 返回设置表是否可用。
func (s *Service) Ready() bool { return s.load().ready }

// ---- 读 ----

// Lookup 返回显式设置的值（敏感项已解密）。没有设置、值为空白、或密文解不开时 ok=false。
func (s *Service) Lookup(key string) (string, bool) {
	e, ok := s.load().m[key]
	if !ok || e.bad || strings.TrimSpace(e.val) == "" {
		return "", false
	}
	return e.val, true
}

// Value 返回生效值：显式设置的值，否则登记的默认值；两者都为空白时 ok=false。
// 各模块原来的 ConfigValue(item) (string, bool) 接口由它实现。
func (s *Service) Value(key string) (string, bool) {
	if v, ok := s.Lookup(key); ok {
		return v, true
	}
	if d := Find(key); d != nil && strings.TrimSpace(d.Default) != "" {
		return d.Default, true
	}
	return "", false
}

// String 返回生效值（没有则空串）。
func (s *Service) String(key string) string { v, _ := s.Value(key); return v }

// Bool 返回生效的布尔值：值不认识时回落到登记的默认值。
func (s *Service) Bool(key string) bool {
	if v, ok := s.Lookup(key); ok {
		if nb, ok := normBool(v); ok {
			return nb == "true"
		}
	}
	if d := Find(key); d != nil {
		nb, _ := normBool(d.Default)
		return nb == "true"
	}
	return false
}

// Int 返回生效的整数：不是整数或超出登记范围时回落到默认值。
func (s *Service) Int(key string) int {
	d := Find(key)
	if v, ok := s.Lookup(key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && (d == nil || !d.Ranged || (float64(n) >= d.Min && float64(n) <= d.Max)) {
			return n
		}
	}
	if d != nil {
		n, _ := strconv.Atoi(strings.TrimSpace(d.Default))
		return n
	}
	return 0
}

// Float 返回生效的小数：不是数字时回落到默认值。
func (s *Service) Float(key string) float64 {
	if v, ok := s.Lookup(key); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f
		}
	}
	if d := Find(key); d != nil {
		f, _ := strconv.ParseFloat(strings.TrimSpace(d.Default), 64)
		return f
	}
	return 0
}

// List 返回生效的列表（逗号分隔，去空白、去空项）。
func (s *Service) List(key string) []string {
	v := s.String(key)
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ---- 页面用的视图 ----

// Item 是页面上的一项。
type Item struct {
	Def
	Value         string     `json:"value"`         // 显式设置的值；敏感项永远为空
	Effective     string     `json:"effective"`     // 生效值（显式值或默认值）；敏感项为空
	Custom        bool       `json:"custom"`        // 是否显式设置过（与默认值相同也算）
	Modified      bool       `json:"modified"`      // 是否与默认值不同
	SecretSet     bool       `json:"secret_set"`    // 敏感项是否已设置
	Undecryptable bool       `json:"undecryptable"` // 库里的密文用当前密钥解不开
	Version       uint       `json:"version"`
	UpdatedBy     string     `json:"updated_by,omitempty"`
	UpdatedAt     *time.Time `json:"updated_at,omitempty"`
	Editable      bool       `json:"editable"`
	EditNote      string     `json:"edit_note,omitempty"`
}

// Items 返回全部登记项（按登记顺序，隐藏项除外）。
func (s *Service) Items() []Item {
	sn := s.load()
	out := make([]Item, 0, len(defs))
	for _, d := range defs {
		if d.Hidden {
			continue
		}
		it := Item{Def: *d, Editable: d.Locked == ""}
		if d.Locked != "" {
			it.EditNote = d.Locked
		}
		if e, ok := sn.m[d.Key]; ok {
			it.Version, it.UpdatedBy = e.row.Version, e.row.UpdatedBy
			if !e.row.UpdatedAt.IsZero() {
				t := e.row.UpdatedAt
				it.UpdatedAt = &t
			}
			it.Custom = true
			it.Undecryptable = e.bad
			switch {
			case d.Secret():
				it.SecretSet = !e.bad && strings.TrimSpace(e.val) != ""
			case e.bad:
			default:
				it.Value = e.val
			}
		}
		if !d.Secret() {
			it.Effective = d.Default
			if it.Custom && strings.TrimSpace(it.Value) != "" {
				it.Effective = it.Value
			}
			it.Modified = it.Custom && strings.TrimSpace(it.Value) != "" && it.Value != d.Default
		} else {
			it.Modified = it.SecretSet
		}
		out = append(out, it)
	}
	return out
}

// Recent 返回变更记录（新的在前）。
func (s *Service) Recent(key string, limit, offset int) ([]History, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return s.store.Recent(key, limit, offset)
}

// ---- 写 ----

// Options 是修改选项。
type Options struct {
	Force  bool   // 允许修改页面里只读（Locked）与隐藏的项，并跳过 Guard 环境校验（命令行用）
	Source string // 变更记录的来源说明（缺省 set）
}

// Normalize 校验并规整要保存的值；空串表示“恢复默认 / 清除”，总是合法。
func (d *Def) Normalize(v string) (string, error) {
	v = strings.TrimSpace(v)
	if err := textClean(v, d.Type == TypeText); err != nil {
		return "", err
	}
	if v == "" {
		return "", nil
	}
	max := 2000
	if d.Type == TypeText {
		max = 60000
	}
	if len([]rune(v)) > max {
		return "", fmt.Errorf("值太长（最多 %d 个字符）", max)
	}
	switch d.Type {
	case TypeBool:
		nb, ok := normBool(v)
		if !ok {
			return "", errors.New("只能是 true 或 false（或留空恢复默认）")
		}
		return nb, nil
	case TypeInt:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return "", errors.New("需要一个整数")
		}
		if d.Ranged && (float64(n) < d.Min || float64(n) > d.Max) {
			return "", fmt.Errorf("需要在 %v 到 %v 之间", d.Min, d.Max)
		}
		v = strconv.FormatInt(n, 10)
	case TypeFloat:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return "", errors.New("需要一个数字")
		}
		if d.Ranged && (f < d.Min || f > d.Max) {
			return "", fmt.Errorf("需要在 %v 到 %v 之间", d.Min, d.Max)
		}
	case TypeEnum:
		vals := make([]string, 0, len(d.Options))
		found := false
		for _, o := range d.Options {
			if o.Value == v {
				found = true
			}
			vals = append(vals, o.Value)
		}
		if !found {
			return "", fmt.Errorf("只能是：%s（或留空恢复默认）", strings.Join(vals, "、"))
		}
	case TypeList:
		v = normList(v)
	}
	if d.Check != nil && v != "" {
		if err := d.Check(v); err != nil {
			return "", err
		}
	}
	return v, nil
}

// Set 修改一项；SetMany 一次修改多项（全部校验通过才写，一个事务）。值为空串 = 恢复默认（敏感项 = 清除）。
func (s *Service) Set(actor Actor, key, value string, opt ...Options) (*Change, error) {
	cs, err := s.SetMany(actor, map[string]string{key: value}, opt...)
	if err != nil {
		return nil, err
	}
	if len(cs) == 0 {
		return nil, nil
	}
	return &cs[0], nil
}

// Reset 恢复默认。
func (s *Service) Reset(actor Actor, key string, opt ...Options) (*Change, error) {
	return s.Set(actor, key, "", opt...)
}

func firstOpt(opt []Options) Options {
	if len(opt) > 0 {
		return opt[0]
	}
	return Options{}
}

// SetMany 的返回值只含真正发生变化的项（值没变的不写库、不记变更）。
func (s *Service) SetMany(actor Actor, kv map[string]string, opt ...Options) ([]Change, error) {
	o := firstOpt(opt)
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	bad := map[string]string{}
	norm := map[string]string{}
	for _, k := range keys {
		d := Find(k)
		switch {
		case d == nil || strings.HasPrefix(k, internalPrefix):
			bad[k] = "没有这个设置项"
			continue
		case d.Hidden && !o.Force:
			bad[k] = "这是内部状态项，不能修改"
			continue
		case d.Locked != "" && !o.Force:
			bad[k] = d.Locked
			continue
		}
		v, err := d.Normalize(kv[k])
		if err != nil {
			bad[k] = err.Error()
			continue
		}
		if g := guardOf(k); g != nil && v != "" && !o.Force {
			if err := g(v); err != nil {
				bad[k] = err.Error()
				continue
			}
		}
		norm[k] = v
	}
	if len(bad) > 0 {
		e := newErr(400, "bad_value", "有 %d 项的值不合法", len(bad))
		e.Fields = bad
		if len(bad) == 1 {
			for k, m := range bad {
				e.Message = k + "：" + m
				if Find(k) == nil {
					e.Status, e.Code = 404, "not_found"
				}
			}
		}
		return nil, e
	}

	s.Invalidate() // 以库里最新的值为准计算变化
	sn := s.load()
	if !sn.ready {
		return nil, newErr(409, "table_missing", "dbt_setting 表不存在：请先执行 deploy/sql/260_setting.sql")
	}
	by := actor.Username
	src := o.Source
	if src == "" {
		src = "set"
	}
	var writes []Write
	var hist []History
	var changes []Change
	for _, k := range keys {
		d := Find(k)
		nv := norm[k]
		cur, has := sn.m[k]
		curVal := ""
		if has && !cur.bad {
			curVal = cur.val
		}
		secret := d.Secret()
		switch {
		case nv == "":
			if !has {
				continue
			}
			writes = append(writes, Write{Key: k, Delete: true, By: by})
			if !d.Hidden {
				hist = append(hist, History{Key: k, Old: ifNot(secret, curVal), Secret: secret, Action: "reset", By: by})
			}
			changes = append(changes, Change{Key: k, Label: d.Label, Old: ifNot(secret, curVal), Secret: secret, Reset: true})
		default:
			if has && !cur.bad && !secret && curVal == nv {
				continue
			}
			stored := nv
			if secret {
				stored = s.ci.Encrypt(nv)
			}
			writes = append(writes, Write{Key: k, Value: stored, Encrypted: secret, By: by})
			if !d.Hidden {
				hist = append(hist, History{Key: k, Old: ifNot(secret, curVal), New: ifNot(secret, nv), Secret: secret, Action: src, By: by})
			}
			changes = append(changes, Change{Key: k, Label: d.Label, Old: ifNot(secret, curVal), New: ifNot(secret, nv), Secret: secret})
		}
	}
	if len(writes) == 0 {
		return nil, nil
	}
	entry := &model.AuditEntry{UserID: actor.ID, UserName: actor.Username, UserDisplay: actor.Display,
		Action: "修改系统设置", ExtraInfo: auditText(changes)}
	if err := s.store.Apply(writes, hist, entry); err != nil {
		return nil, err
	}
	s.Invalidate()
	return changes, nil
}

func ifNot(secret bool, v string) string {
	if secret {
		return ""
	}
	return v
}

// auditText 是写进系统审计的说明：敏感项只记“已更新”，不记内容。
func auditText(cs []Change) string {
	parts := make([]string, 0, len(cs))
	show := func(v string) string {
		r := []rune(v)
		if len(r) > 120 {
			return string(r[:120]) + "…"
		}
		return v
	}
	for _, c := range cs {
		name := c.Key
		if c.Label != "" {
			name = fmt.Sprintf("%s（%s）", c.Key, c.Label)
		}
		switch {
		case c.Secret && c.Reset:
			parts = append(parts, name+"：已清除（内容不记录）") // i18n-ignore 存库的审计说明
		case c.Secret:
			parts = append(parts, name+"：已更新（内容不记录）") // i18n-ignore 存库的审计说明
		case c.Reset:
			parts = append(parts, fmt.Sprintf("%s：%s → 默认值", name, orEmpty(show(c.Old)))) // i18n-ignore 存库的审计说明
		default:
			parts = append(parts, fmt.Sprintf("%s：%s → %s", name, orEmpty(show(c.Old)), orEmpty(show(c.New))))
		}
	}
	out := strings.Join(parts, "；")
	if r := []rune(out); len(r) > 1500 {
		out = string(r[:1500]) + "…"
	}
	return out
}

func orEmpty(v string) string {
	if v == "" {
		return "（空）"
	}
	return v
}

// ---- 供各模块在自己的事务里写（模块自己写审计）----

// Prepare 校验并生成写入指令，不落库：模块用它在自己的事务里 WriteTx（同时写自己的审计），提交后调用 Invalidate。
// 与 SetMany 的区别：不写系统审计、不要求 Actor 是超级用户，也不拒绝 Locked 项以外的页面限制之外的东西。
func (s *Service) Prepare(by string, kv map[string]string) ([]Write, []History, error) {
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sn := s.load()
	var ws []Write
	var hs []History
	for _, k := range keys {
		d := Find(k)
		if d == nil {
			return nil, nil, fmt.Errorf("没有这个设置项：%s", k)
		}
		nv, err := d.Normalize(kv[k])
		if err != nil {
			return nil, nil, fmt.Errorf("%s：%w", k, err)
		}
		cur, has := sn.m[k]
		curVal := ""
		if has && !cur.bad {
			curVal = cur.val
		}
		secret := d.Secret()
		if nv == "" {
			if has {
				ws = append(ws, Write{Key: k, Delete: true, By: by})
				if !d.Hidden {
					hs = append(hs, History{Key: k, Old: ifNot(secret, curVal), Secret: secret, Action: "reset", By: by})
				}
			}
			continue
		}
		if has && !cur.bad && !secret && curVal == nv {
			continue
		}
		stored := nv
		if secret {
			stored = s.ci.Encrypt(nv)
		}
		ws = append(ws, Write{Key: k, Value: stored, Encrypted: secret, By: by})
		if !d.Hidden {
			hs = append(hs, History{Key: k, Old: ifNot(secret, curVal), New: ifNot(secret, nv), Secret: secret, Action: "set", By: by})
		}
	}
	return ws, hs, nil
}
