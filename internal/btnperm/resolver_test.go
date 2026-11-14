// @author: kevinkelly
// @license: Apache Licence
// @file: resolver_test.go
// @time: 2026/10/28

package btnperm

import (
	"testing"
	"time"
)

func fixedNow() time.Time { return time.Date(2026, 10, 6, 12, 0, 0, 0, time.Local) }

func newRes(f *fakeRepo) *Resolver {
	r := New(f)
	r.SetClock(fixedNow)
	return r
}

func eq(t *testing.T, what string, got, want bool) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, 期望 %v", what, got, want)
	}
}

// 判定矩阵：权限只认按钮——权限组授予 + 单用户授权 / 禁用；停用用户什么都没有；基础按钮登录即可；原权限码不起任何作用。
func TestButtonMatrix(t *testing.T) {
	f := newFake()
	f.addUser(1, "alice", true, false) // 只有早期的原权限码（没迁移过），没有任何按钮授权
	f.addUser(2, "bob", true, false)   // 权限组授予
	f.addUser(3, "carl", true, false)  // 没有任何权限
	f.addUser(4, "off", false, false)  // 已停用，且在 bob 的组里
	f.giveDjango(1, "sql_review", "sql_execute", "menu_sqlworkflow")
	f.addGroup(10, "RD", 2, 4)
	f.grant(10, "sqlworkflow.view_own", "sqlworkflow.execute_group")
	r := newRes(f)

	eq(t, "alice approve（只有原权限码 sql_review：不再生效）", ok(r.HasButton(1, "sqlworkflow.approve")), false)
	eq(t, "alice schedule（只有原权限码 sql_execute）", ok(r.HasButton(1, "sqlworkflow.schedule")), false)
	eq(t, "alice view_own（只有原权限码 menu_sqlworkflow）", ok(r.HasButton(1, "sqlworkflow.view_own")), false)
	eq(t, "alice cancel_own（基础按钮，登录即可）", ok(r.HasButton(1, "sqlworkflow.cancel_own")), true)
	eq(t, "alice rollback（基础按钮）", ok(r.HasButton(1, "sqlworkflow.rollback")), true)
	eq(t, "alice qpriv.apply（基础按钮）", ok(r.HasButton(1, "qpriv.apply")), true)
	eq(t, "alice skip_check（仅超管，不可授予）", ok(r.HasButton(1, "sqlworkflow.skip_check")), false)
	eq(t, "alice inspect.run（默认仅超管）", ok(r.HasButton(1, "inspect.run")), false)
	eq(t, "bob execute_group（权限组）", ok(r.HasButton(2, "sqlworkflow.execute_group")), true)
	eq(t, "bob view_own（权限组）", ok(r.HasButton(2, "sqlworkflow.view_own")), true)
	eq(t, "bob approve（没授予）", ok(r.HasButton(2, "sqlworkflow.approve")), false)
	eq(t, "carl view_own", ok(r.HasButton(3, "sqlworkflow.view_own")), false)
	eq(t, "carl 基础按钮", ok(r.HasButton(3, "sqlworkflow.cancel_own")), true)
	eq(t, "停用用户（同组）", ok(r.HasButton(4, "sqlworkflow.view_own")), false)
	eq(t, "停用用户的基础按钮", ok(r.HasButton(4, "sqlworkflow.cancel_own")), false)
	eq(t, "不存在的用户", ok(r.HasButton(99, "sqlworkflow.cancel_own")), false)
	eq(t, "未登记的码", ok(r.HasButton(1, "no.such")), false)

	// 单用户授权 / 禁用
	f.overrides[1] = []Override{{Code: "sqlworkflow.approve", Effect: EffectAllow}}
	f.overrides[2] = []Override{{Code: "sqlworkflow.execute_group", Effect: EffectDeny}}
	f.overrides[3] = []Override{{Code: "sqlworkflow.cancel_own", Effect: EffectDeny}}
	r.InvalidateAll()
	eq(t, "alice 单独授权", ok(r.HasButton(1, "sqlworkflow.approve")), true)
	eq(t, "bob 单独禁用压过权限组", ok(r.HasButton(2, "sqlworkflow.execute_group")), false)
	eq(t, "carl 基础按钮也能被单独禁用", ok(r.HasButton(3, "sqlworkflow.cancel_own")), false)
}

// 单用户禁用优先，到期后失效；单独授权同理。
func TestOverridesDenyAllowExpire(t *testing.T) {
	f := newFake()
	f.addUser(1, "alice", true, false)
	f.addGroup(10, "RD", 1)
	f.grant(10, "sqlworkflow.approve", "sqlworkflow.schedule", "export.download")
	r := newRes(f)
	now := fixedNow()
	future, past := now.Add(24*time.Hour), now.Add(-time.Hour)

	f.overrides[1] = []Override{
		{Code: "export.download", Effect: EffectDeny},                                                 // 永久禁用：压过权限组
		{Code: "sqlworkflow.approve", Effect: EffectDeny, ExpireAt: &past},                            // 已到期的禁用：无效
		{Code: "sqlworkflow.schedule", Effect: EffectDeny, ExpireAt: &future},                         // 未到期的禁用：有效
		{Code: "perf.create", Effect: EffectAllow, ExpireAt: &future},                                 // 未到期的授权
		{Code: "perf.assign", Effect: EffectAllow, ExpireAt: &past},                                   // 已到期的授权：无效
		{Code: "query.admin_run", Effect: EffectAllow}, {Code: "query.admin_run", Effect: EffectDeny}, // 同码授权 + 禁用：禁用优先
	}
	r.InvalidateAll()
	eq(t, "deny 压过权限组", ok(r.HasButton(1, "export.download")), false)
	eq(t, "到期的禁用无效", ok(r.HasButton(1, "sqlworkflow.approve")), true)
	eq(t, "有效禁用", ok(r.HasButton(1, "sqlworkflow.schedule")), false)
	eq(t, "单独授权", ok(r.HasButton(1, "perf.create")), true)
	eq(t, "到期的授权无效", ok(r.HasButton(1, "perf.assign")), false)
	eq(t, "同码授权+禁用", ok(r.HasButton(1, "query.admin_run")), false)

	// 时间推进：未到期变到期
	f.overrides[1] = []Override{{Code: "export.download", Effect: EffectDeny, ExpireAt: &future}}
	r.InvalidateAll()
	eq(t, "到期前", ok(r.HasButton(1, "export.download")), false)
	r.SetClock(func() time.Time { return future.Add(time.Minute) })
	r.InvalidateAll()
	eq(t, "到期后恢复", ok(r.HasButton(1, "export.download")), true)
}

// 迁移状态（mode）只是标记：不管是 compat 还是 strict，判定都一样——原权限码从不参与。
func TestModeDoesNotChangeEvaluation(t *testing.T) {
	f := newFake()
	f.addUser(1, "alice", true, false)
	f.giveDjango(1, "sql_review", "menu_sqlworkflow", "query_all_instances")
	f.addGroup(10, "RD", 1)
	f.grant(10, "sqlworkflow.view_own")
	for _, mode := range []string{ModeCompat, ModeStrict, ""} {
		f.setting.Mode = mode
		r := newRes(f)
		eq(t, mode+" 权限组授予", ok(r.HasButton(1, "sqlworkflow.view_own")), true)
		eq(t, mode+" 原权限码不放行", ok(r.HasButton(1, "sqlworkflow.approve")), false)
		eq(t, mode+" 原权限码不放行（全部实例）", ok(r.HasButton(1, "query.admin_run")), false)
		eq(t, mode+" 基础按钮", ok(r.HasButton(1, "sqlworkflow.cancel_own")), true)
	}
}

// 多个权限组取并集；Locked 按钮永远不能通过解析器放行；解析器不处理超级用户（调用方先行判断）。
func TestMultiGroupUnionAndLocked(t *testing.T) {
	f := newFake()
	f.addUser(1, "alice", true, false)
	f.addUser(2, "root", true, true)
	f.addGroup(10, "RD", 1)
	f.addGroup(11, "TL", 1)
	f.grant(10, "sqlworkflow.view_own", "sqlworkflow.submit")
	f.grant(11, "sqlworkflow.approve", "user.edit") // user.edit 是 Locked，即使写进了表也不放行
	r := newRes(f)
	eq(t, "并集 submit", ok(r.HasButton(1, "sqlworkflow.submit")), true)
	eq(t, "并集 approve", ok(r.HasButton(1, "sqlworkflow.approve")), true)
	eq(t, "Locked", ok(r.HasButton(1, "user.edit")), false)
	eq(t, "没有的", ok(r.HasButton(1, "sqlworkflow.osc")), false)
	got := ok(r.AllowedButtons(1))
	if !got["sqlworkflow.submit"] || !got["sqlworkflow.approve"] || got["user.edit"] || !got["sqlworkflow.cancel_own"] {
		t.Errorf("AllowedButtons = %v", got)
	}
	// Explain：来源包含权限组名
	ds, _, err := newRes(f).Explain(1)
	if err != nil {
		t.Fatal(err)
	}
	if d := ds["sqlworkflow.submit"]; !d.Allowed || d.Source != SrcGroup || len(d.Groups) != 1 || d.Groups[0] != "RD" {
		t.Errorf("submit 来源 = %+v", d)
	}
	if d := ds["user.edit"]; d.Allowed || d.Source != SrcLocked {
		t.Errorf("Locked 来源 = %+v", d)
	}
	if d := ds["sqlworkflow.cancel_own"]; !d.Allowed || d.Source != SrcOpen {
		t.Errorf("基础按钮来源 = %+v", d)
	}
	// 超级用户在 Explain 里全部放行
	ds, _, _ = newRes(f).Explain(2)
	if d := ds["user.edit"]; !d.Allowed || d.Source != SrcSuper {
		t.Errorf("超级用户 = %+v", d)
	}
}

// 缓存：命中、写后失效、TTL 过期、上限。
func TestCache(t *testing.T) {
	f := newFake()
	f.addUser(1, "alice", true, false)
	f.addGroup(10, "RD", 1)
	f.grant(10, "sqlworkflow.view_own")
	r := newRes(f)
	now := fixedNow()
	clock := now
	r.SetClock(func() time.Time { return clock })

	for i := 0; i < 5; i++ {
		eq(t, "命中", ok(r.HasButton(1, "sqlworkflow.view_own")), true)
	}
	if f.profileHit != 1 {
		t.Fatalf("5 次判断应只加载 1 次 Profile，实际 %d", f.profileHit)
	}
	// 写后失效
	f.groupBtns[10] = map[string]bool{}
	eq(t, "缓存期内仍是旧值", ok(r.HasButton(1, "sqlworkflow.view_own")), true)
	r.InvalidateAll()
	eq(t, "失效后立即生效", ok(r.HasButton(1, "sqlworkflow.view_own")), false)
	if f.profileHit != 2 {
		t.Fatalf("Profile 加载次数 = %d", f.profileHit)
	}
	// TTL 过期
	f.grant(10, "sqlworkflow.view_own")
	clock = clock.Add(DefaultTTL + time.Second)
	eq(t, "TTL 过期后重新加载", ok(r.HasButton(1, "sqlworkflow.view_own")), true)
	// 单用户失效
	f.groupBtns[10] = map[string]bool{}
	r.InvalidateUser(1)
	eq(t, "InvalidateUser", ok(r.HasButton(1, "sqlworkflow.view_own")), false)

	// 上限：超过 max 后缓存不会无限增长
	r.SetCache(time.Hour, 3)
	for id := uint(100); id < 120; id++ {
		f.addUser(id, "u", true, false)
		_, _ = r.HasButton(id, "sqlworkflow.view_own")
		if n := r.CacheSize(); n > 3 {
			t.Fatalf("缓存超过上限: %d", n)
		}
	}
	// ttl<=0 不缓存
	r.SetCache(0, 10)
	before := f.profileHit
	_, _ = r.HasButton(1, "sqlworkflow.view_own")
	_, _ = r.HasButton(1, "sqlworkflow.view_own")
	if f.profileHit-before != 2 || r.CacheSize() != 0 {
		t.Fatal("ttl=0 不应缓存")
	}
}

// 加载期间发生失效：旧结果不能写进缓存。
type slowSource struct {
	*fakeRepo
	onProfile func()
}

func (s *slowSource) Profile(uid uint) (*Profile, error) {
	p, err := s.fakeRepo.Profile(uid)
	if s.onProfile != nil {
		s.onProfile()
	}
	return p, err
}

func TestInvalidateDuringLoad(t *testing.T) {
	f := newFake()
	f.addUser(1, "alice", true, false)
	f.addGroup(10, "RD", 1)
	f.grant(10, "sqlworkflow.view_own")
	src := &slowSource{fakeRepo: f}
	r := New(src)
	src.onProfile = func() { r.InvalidateAll(); src.onProfile = nil }
	_, _ = r.HasButton(1, "sqlworkflow.view_own")
	if r.CacheSize() != 0 {
		t.Fatal("失效发生在加载期间，结果不应入缓存")
	}
}

// 设置缓存与默认值：读取失败时返回默认设置与错误。
func TestSettingCacheAndError(t *testing.T) {
	f := newFake()
	f.addUser(1, "alice", true, false)
	r := newRes(f)
	s := ok(r.Setting())
	if s.Mode != ModeCompat || s.DenyDisplay != DenyHide {
		t.Fatalf("默认设置 = %+v", s)
	}
	f.setting = Setting{Mode: ModeStrict, DenyDisplay: DenyDisable}
	if ok(r.Setting()).Mode != ModeCompat {
		t.Fatal("缓存期内应沿用旧设置")
	}
	r.InvalidateAll()
	if got := ok(r.Setting()); got.Mode != ModeStrict || got.DenyDisplay != DenyDisable {
		t.Fatalf("失效后 = %+v", got)
	}
	f.settingErr = errBoom
	r.InvalidateAll()
	if got, err := r.Setting(); err == nil || got != DefaultSetting() {
		t.Fatalf("读取设置失败应返回错误与默认设置: %+v %v", got, err)
	}
	// 按钮判定不依赖设置：设置读不出来也照常判断
	if !ok(r.HasButton(1, "sqlworkflow.cancel_own")) {
		t.Fatal("按钮判定不应依赖全局设置")
	}
	// 非法值收敛为默认
	if (Setting{Mode: "x", DenyDisplay: "y"}).Normalize() != DefaultSetting() {
		t.Fatal("Normalize")
	}
}

type boomErr struct{}

func (boomErr) Error() string { return "boom" }

var errBoom = boomErr{}

// ok 取出 (值, error) 的值；出错直接 panic（测试里等同于失败）。
func ok[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
