// @author: kevinkelly
// @license: Apache Licence
// @file: settings_test.go
// @time: 2026/10/28

package settings

import (
	"errors"
	"strings"
	"testing"
	"time"

	"dbtool/internal/crypto"
)

func testCipher(t *testing.T) *crypto.Mirage {
	t.Helper()
	m, err := crypto.NewMirage("0123456789abcdef0123456789abcdef", "ECB", "")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

var admin = Actor{ID: 1, Username: "admin", Display: "管理员"}

func TestRegistrySane(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range Defs() {
		if seen[d.Key] {
			t.Errorf("重复的键 %s", d.Key)
		}
		seen[d.Key] = true
		if !strings.Contains(d.Key, ".") {
			t.Errorf("%s 缺少领域前缀", d.Key)
		}
		known := false
		for _, g := range Groups {
			if g.Key == d.Group {
				known = true
			}
		}
		if !known {
			t.Errorf("%s 的分组 %q 不在 Groups 里", d.Key, d.Group)
		}
		if d.Type == TypeSecret && d.Default != "" {
			t.Errorf("敏感项 %s 不能有默认值", d.Key)
		}
	}
}

func TestTypedGettersUseDefaults(t *testing.T) {
	s := NewMemory(nil)
	if got := s.Int(KeySecurityLoginLockAttempts); got != 5 {
		t.Errorf("默认锁定次数 = %d，应为 5", got)
	}
	if !s.Bool(KeyApprovalForbidSelf) {
		t.Error("禁止自审默认应为开启")
	}
	if s.Bool(KeySecuritySelfSignup) {
		t.Error("自助注册默认应为关闭")
	}
	if v, ok := s.Value(KeyGoInceptionHost); ok || v != "" {
		t.Errorf("没有默认值且未设置的项 Value 应返回 ok=false，得到 %q %v", v, ok)
	}
	if got := s.List(KeySecurityExternalMatchBy); len(got) != 2 || got[0] != "username" || got[1] != "email" {
		t.Errorf("匹配依据默认值 = %v", got)
	}
	if got := s.Float(KeyPerfVerifyImproveRatio); got != 0.5 {
		t.Errorf("改善比例默认 = %v", got)
	}
}

func TestSetValidatesAndNormalizes(t *testing.T) {
	s := NewMemory(nil)
	cases := []struct {
		key, val string
		ok       bool
	}{
		{KeyQueryMaxExecSeconds, "30", true},
		{KeyQueryMaxExecSeconds, "0", false},
		{KeyQueryMaxExecSeconds, "abc", false},
		{KeyQueryMaxExecSeconds, "99999", false},
		{KeyQueryForbidSelectStar, "True", true},
		{KeyQueryForbidSelectStar, "maybe", false},
		{KeyFirewallMode, "enforce", true},
		{KeyFirewallMode, "block", false},
		{KeyReviewCriticalDDLRegex, "^drop (", false},
		{KeyReviewCriticalDDLRegex, "^drop ", true},
		{KeyRetentionWindow, "25:00-03:00", false},
		{KeyRetentionWindow, "01:00-05:00", true},
		{KeyNotifyPhases, "Pass,Bad", false},
		{KeyNotifyPhases, " Pass , Execute ", true},
		{KeySecurityAPIUserIDs, "3, 15,x", false},
		{"no.such_key", "1", false},
		{KeySecurityLocalLastLogin, "x", false}, // 隐藏项页面不能改
		{KeyOptimizerSoarPath, "/usr/bin/soar", true},
		{KeyOptimizerSoarPath, "/usr/local/bin/soar.exe", true},
		{KeyOptimizerSoarPath, "soar", false},               // 必须是绝对路径
		{KeyOptimizerSoarPath, "/bin/sh", false},            // 文件名必须是 soar
		{KeyOptimizerSoarPath, "/opt/x/../bin/soar", false}, // 不能有 ..
		{KeyOptimizerSoarConfig, "/etc/soar.yaml", true},
		{KeyOptimizerSoarConfig, "etc/soar.yaml", false},
		{KeyOptimizerSoarTestDSN, "u:p@10.0.0.9:3307/test", true},
		{KeyOptimizerSoarTestDSN, "10.0.0.9:3307/test", false},
		{KeyOptimizerSoarTestDSN, "mysql://u:p@10.0.0.9/test", false},
		{KeyOptimizerSoarTestDSN, "u:p@10.0.0.9:99999/test", false},
	}
	for _, c := range cases {
		_, err := s.Set(admin, c.key, c.val)
		if (err == nil) != c.ok {
			t.Errorf("Set(%s, %q) err=%v，期望 ok=%v", c.key, c.val, err, c.ok)
		}
	}
	if v, _ := s.Lookup(KeyQueryForbidSelectStar); v != "true" {
		t.Errorf("布尔值应规整为 true，得到 %q", v)
	}
	if got := s.List(KeyNotifyPhases); len(got) != 2 || got[1] != "Execute" {
		t.Errorf("列表应去空白：%v", got)
	}
}

func TestSetManyIsAtomic(t *testing.T) {
	s := NewMemory(nil)
	_, err := s.SetMany(admin, map[string]string{KeyQueryMaxExecSeconds: "20", KeyFirewallMode: "bogus"})
	if err == nil {
		t.Fatal("有一项不合法时应整体失败")
	}
	e := err.(*Error)
	if len(e.Fields) != 1 || e.Fields[KeyFirewallMode] == "" {
		t.Errorf("应逐项指出错误：%+v", e.Fields)
	}
	if _, ok := s.Lookup(KeyQueryMaxExecSeconds); ok {
		t.Error("整体失败后合法项也不应写入")
	}
}

func TestSetNoopAndReset(t *testing.T) {
	ms := NewMemStore()
	s := New(ms, PlainCipher{})
	if cs, err := s.SetMany(admin, map[string]string{KeyQueryMaxExecSeconds: "30"}); err != nil || len(cs) != 1 {
		t.Fatalf("首次设置: %v %v", cs, err)
	}
	if cs, err := s.SetMany(admin, map[string]string{KeyQueryMaxExecSeconds: "30"}); err != nil || len(cs) != 0 {
		t.Errorf("值没变不应产生变更: %v %v", cs, err)
	}
	if cs, _ := s.SetMany(admin, map[string]string{KeyQueryMaxExecSeconds: ""}); len(cs) != 1 || !cs[0].Reset || cs[0].Old != "30" {
		t.Errorf("恢复默认: %+v", cs)
	}
	if _, ok := s.Lookup(KeyQueryMaxExecSeconds); ok {
		t.Error("恢复默认后不应再有显式值")
	}
	if got := s.Int(KeyQueryMaxExecSeconds); got != 60 {
		t.Errorf("恢复默认后应取默认值 60，得到 %d", got)
	}
	if cs, _ := s.SetMany(admin, map[string]string{KeyQueryMaxExecSeconds: ""}); len(cs) != 0 {
		t.Error("本来就是默认，恢复默认不应产生变更")
	}
	h, _ := s.Recent(KeyQueryMaxExecSeconds, 10, 0)
	if len(h) != 2 || h[0].Action != "reset" || h[1].Action != "set" || h[1].New != "30" {
		t.Errorf("变更记录: %+v", h)
	}
	if n := len(ms.Audits()); n != 2 {
		t.Errorf("每次实际修改写一条审计，得到 %d", n)
	}
}

func TestSecretsAreEncryptedAndNeverLogged(t *testing.T) {
	ms := NewMemStore()
	m := testCipher(t)
	s := New(ms, m)
	if _, err := s.Set(admin, KeyGoInceptionPassword, "p@ss-word"); err != nil {
		t.Fatal(err)
	}
	rows, _, _ := ms.Load()
	r := rows[KeyGoInceptionPassword]
	if !r.Encrypted || r.Value == "p@ss-word" || !crypto.LooksEncrypted(r.Value) {
		t.Errorf("敏感值应加密保存: %+v", r)
	}
	if v, ok := s.Lookup(KeyGoInceptionPassword); !ok || v != "p@ss-word" {
		t.Errorf("读取应解密: %q %v", v, ok)
	}
	for _, a := range ms.Audits() {
		if strings.Contains(a.ExtraInfo, "p@ss-word") {
			t.Errorf("审计里不能出现密钥内容: %s", a.ExtraInfo)
		}
	}
	h, _ := s.Recent(KeyGoInceptionPassword, 10, 0)
	if len(h) != 1 || h[0].New != "" || !h[0].Secret {
		t.Errorf("敏感项变更记录不能带值: %+v", h)
	}
	for _, it := range s.Items() {
		if it.Key == KeyGoInceptionPassword {
			if it.Value != "" || it.Effective != "" || !it.SecretSet {
				t.Errorf("页面视图不能回显密钥: %+v", it)
			}
		}
	}
	// 重新保存同一个值也要写（不能比较密文）；清除后 SecretSet=false
	if cs, _ := s.Set(admin, KeyGoInceptionPassword, ""); cs == nil || !cs.Reset {
		t.Errorf("清除: %+v", cs)
	}
}

// withLockedDef 临时登记一个只读项（现在登记表里没有真正的只读项，机制本身仍要测）。
func withLockedDef(t *testing.T) string {
	t.Helper()
	const key = "test.locked_item"
	byKey[key] = &Def{Key: key, Group: GroupOptimizer, Label: "只读项", Type: TypeString, Locked: "测试用"}
	t.Cleanup(func() { delete(byKey, key) })
	return key
}

func TestForceOnLockedAndHidden(t *testing.T) {
	s := NewMemory(nil)
	key := withLockedDef(t)
	if _, err := s.Set(admin, key, "x"); err == nil {
		t.Error("没有 Force 不应能改只读项")
	}
	if _, err := s.Set(admin, key, "x", Options{Force: true}); err != nil {
		t.Errorf("Force 应允许改只读项: %v", err)
	}
	if v, _ := s.Lookup(key); v != "x" {
		t.Errorf("得到 %q", v)
	}
	if _, err := s.Set(admin, KeySecurityLocalLastLogin, "2026-01-01 10:00:00", Options{Force: true}); err != nil {
		t.Errorf("Force 应允许改隐藏项: %v", err)
	}
	if _, err := s.Set(admin, internalPrefix+"x", "1", Options{Force: true}); err == nil {
		t.Error("内部键任何情况下都不能通过 Set 修改")
	}
}

func TestCacheTTLAndInvalidate(t *testing.T) {
	ms := NewMemStore()
	s := New(ms, PlainCipher{})
	clock := time.Now()
	s.now = func() time.Time { return clock }
	if v, ok := s.Lookup(KeyGoInceptionHost); ok {
		t.Fatalf("初始应没有值: %q", v)
	}
	ms.Put(KeyGoInceptionHost, "gi.example.com", false) // 绕过服务直接改库
	if _, ok := s.Lookup(KeyGoInceptionHost); ok {
		t.Error("TTL 内应读到缓存里的旧快照")
	}
	clock = clock.Add(DefaultTTL + time.Second)
	if v, ok := s.Lookup(KeyGoInceptionHost); !ok || v != "gi.example.com" {
		t.Errorf("TTL 过后应重新加载: %q %v", v, ok)
	}
	ms.Put(KeyGoInceptionHost, "gi2.example.com", false)
	s.Invalidate()
	if v, _ := s.Lookup(KeyGoInceptionHost); v != "gi2.example.com" {
		t.Errorf("Invalidate 后应立即重新加载: %q", v)
	}
}

func TestNotReadyBehavesAsDefaults(t *testing.T) {
	ms := NewMemStore()
	ms.NotReady = true
	s := New(ms, PlainCipher{})
	if s.Ready() {
		t.Error("表不存在时 Ready 应为 false")
	}
	if got := s.Int(KeyQueryMaxExecSeconds); got != 60 {
		t.Errorf("表不存在时读取按默认值: %d", got)
	}
	_, err := s.Set(admin, KeyQueryMaxExecSeconds, "10")
	if e, ok := err.(*Error); !ok || e.Code != "table_missing" {
		t.Errorf("表不存在时修改应返回 table_missing: %v", err)
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func containsPrefix(list []string, p string) bool {
	for _, x := range list {
		if strings.HasPrefix(x, p) {
			return true
		}
	}
	return false
}

func TestGuardRunsForPageNotForce(t *testing.T) {
	s := NewMemory(nil)
	called := 0
	RegisterGuard(KeyOptimizerSoarPath, func(v string) error {
		called++
		return errors.New("环境不满足")
	})
	t.Cleanup(func() { RegisterGuard(KeyOptimizerSoarPath, nil) })

	if _, err := s.Set(admin, KeyOptimizerSoarPath, "/opt/soar"); err == nil || !strings.Contains(err.Error(), "环境不满足") {
		t.Fatalf("页面保存应被 Guard 拒绝: %v", err)
	}
	if v, ok := s.Lookup(KeyOptimizerSoarPath); ok {
		t.Fatalf("被拒绝后不应保存: %q", v)
	}
	if _, err := s.Set(admin, KeyOptimizerSoarPath, "/opt/soar", Options{Force: true}); err != nil {
		t.Fatalf("Force 应跳过 Guard: %v", err)
	}
	n := called
	if _, err := s.Set(admin, KeyOptimizerSoarPath, ""); err != nil {
		t.Fatalf("清除不应受 Guard 限制: %v", err)
	}
	if called != n {
		t.Error("清除（空值）不应调用 Guard")
	}
	// 写法不合法时先被 Check 拒绝，不会走到 Guard
	n = called
	if _, err := s.Set(admin, KeyOptimizerSoarPath, "/bin/sh"); err == nil || called != n {
		t.Errorf("写法不合法应先被拒绝: err=%v called=%d→%d", err, n, called)
	}
}

func TestSoarDSNAddr(t *testing.T) {
	cases := []struct {
		dsn  string
		host string
		port int
		ok   bool
	}{
		{"u:p@10.0.0.9:3307/test", "10.0.0.9", 3307, true},
		{"u:p@db.example.com/test", "db.example.com", 3306, true},
		{"u:p@[::1]:3306/x", "::1", 3306, true},
		{"u:p:with:colons@h:1/x", "h", 1, true},
		{"u@h", "h", 3306, true},
		{"u:p@h:0/x", "", 0, false},
		{"u:p@h:abc/x", "", 0, false},
		{"@h:3306/x", "", 0, false},
		{"u:p@/x", "", 0, false},
		{"h:3306/x", "", 0, false},
		{"mysql://u:p@h:3306/x", "", 0, false},
	}
	for _, c := range cases {
		h, p, err := SoarDSNAddr(c.dsn)
		if (err == nil) != c.ok || (c.ok && (h != c.host || p != c.port)) {
			t.Errorf("SoarDSNAddr(%q) = %q, %d, %v；期望 %q, %d, ok=%v", c.dsn, h, p, err, c.host, c.port, c.ok)
		}
	}
}
