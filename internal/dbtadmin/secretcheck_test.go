// @author: kevinkelly
// @license: Apache Licence
// @file: secretcheck_test.go
// @time: 2026/10/28

package dbtadmin

import (
	"errors"
	"strings"
	"testing"

	"dbtool/internal/config"
	"dbtool/internal/crypto"
)

// 测试用的“新系统”密钥（>= 32 字符）：与公开缺省值不同。
const (
	testNewKey = "n3w-k3y-for-tests-0123456789abcdefghijklmnopqrstuvwxyz"
	testOldKey = "0ld-s3cret-key-used-by-some-earlier-deployment-0123456789"
)

func mustMirage(t *testing.T, key string) *crypto.Mirage {
	t.Helper()
	m, err := crypto.NewMirage(key, "ECB", "1234567890abcdef")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// 之前的部署没配密钥（用公开缺省 SECRET_KEY 加密），新系统部署用了另一把密钥：
// 配置项与实例账号 / 密码都解不开，用缺省密钥才解得开；输出指出修法，且不泄露任何密钥或明文。
func TestCheckSecretsFindsValuesEncryptedWithPublicDefaultKey(t *testing.T) {
	old := mustMirage(t, config.DefaultSecretKey())
	st := newMemStore()
	st.secrets = []SecretRow{
		{Column: ColSettingValue, Ref: "storage_type", Value: old.Encrypt("local")},
		{Column: ColSettingValue, Ref: "max_export_rows", Value: old.Encrypt("500000")},
		{Column: ColSettingValue, Ref: "data_masking", Value: "true"}, // 明文
		{Column: ColInstanceUser, Ref: "prod-mysql-01", Value: old.Encrypt("dba_user_xyz")},
		{Column: ColInstancePassword, Ref: "prod-mysql-01", Value: old.Encrypt("P@ssw0rd-very-secret")},
		{Column: ColInstancePassword, Ref: "empty-pw", Value: ""}, // 空值不计
	}
	te := newTestEnv(t, st)
	te.env["SECRET_KEY"] = testNewKey
	te.env["MIRAGE_SECRET_KEY"] = testNewKey

	if code := te.run("check-secrets"); code != ExitFailed {
		t.Fatalf("有解不开的值应返回 1: %d\n%s", code, te.all())
	}
	out := te.all()
	mustContain(t, "输出", out, "加密密钥不一致" /* 结论 */, "仅旧密钥可解", "公开缺省密钥", "storage_type", "max_export_rows", "prod-mysql-01",
		"MIRAGE_SECRET_KEY", "SECRET_KEY 保持不变", "用户登录不受影响", "PBKDF2")
	for _, secret := range []string{testNewKey, config.DefaultSecretKey(), "local", "500000", "dba_user_xyz", "P@ssw0rd-very-secret", old.Encrypt("local"), old.Encrypt("P@ssw0rd-very-secret")} {
		if strings.Contains(out, secret) {
			t.Fatalf("输出里不能出现密钥、密文或明文值: %q", secret)
		}
	}
	// 明文那项不算问题；统计行：dbt_setting 合计 3、明文 1、仅旧密钥可解 2
	if !strings.Contains(out, ColSettingValue) || !strings.Contains(out, ColInstancePassword) {
		t.Errorf("应列出各检查对象: %s", out)
	}
}

// 新旧密钥一致：全部解得开 → 返回 0。
func TestCheckSecretsAllGood(t *testing.T) {
	cur := mustMirage(t, testNewKey)
	st := newMemStore()
	st.secrets = []SecretRow{
		{Column: ColSettingValue, Ref: "storage_type", Value: cur.Encrypt("local")},
		{Column: ColInstanceUser, Ref: "a", Value: cur.Encrypt("u")},
		{Column: ColInstancePassword, Ref: "a", Value: cur.Encrypt("p")},
		{Column: ColSettingValue, Ref: "query_check", Value: "true"},
	}
	te := newTestEnv(t, st)
	te.env["SECRET_KEY"] = testNewKey
	if code := te.run("check-secrets"); code != ExitOK {
		t.Fatalf("全部可解应返回 0: %d\n%s", code, te.all())
	}
	mustContain(t, "输出", te.all(), "密钥与数据一致")
}

// 混合状态：一部分是旧缺省密钥加密的，一部分是新系统用新密钥保存的：提示改密钥会让后者解不开。
func TestCheckSecretsMixedState(t *testing.T) {
	old, cur := mustMirage(t, config.DefaultSecretKey()), mustMirage(t, testNewKey)
	st := newMemStore()
	st.secrets = []SecretRow{
		{Column: ColInstancePassword, Ref: "imported", Value: old.Encrypt("old-pw")},
		{Column: ColInstancePassword, Ref: "added-in-new-system", Value: cur.Encrypt("new-pw")},
	}
	te := newTestEnv(t, st)
	te.env["MIRAGE_SECRET_KEY"] = testNewKey
	te.env["SECRET_KEY"] = "another-random-secret-key-for-sessions-0123456789"
	if code := te.run("check-secrets"); code != ExitFailed {
		t.Fatalf("%d\n%s", code, te.all())
	}
	out := te.all()
	mustContain(t, "混合", out, "imported", "混合状态", "重新录入")
	if strings.Contains(out, "added-in-new-system") {
		t.Errorf("当前密钥解得开的项不应列在“需要关注”里: %s", out)
	}
}

// 谁都解不开：提示之前的部署可能自配过密钥 / 模式 / IV。
func TestCheckSecretsUnknownKey(t *testing.T) {
	other := mustMirage(t, testOldKey)
	st := newMemStore()
	st.secrets = []SecretRow{{Column: ColSettingValue, Ref: "storage_type", Value: other.Encrypt("local")}}
	te := newTestEnv(t, st)
	te.env["SECRET_KEY"] = testNewKey
	if code := te.run("check-secrets"); code != ExitFailed {
		t.Fatalf("%d\n%s", code, te.all())
	}
	mustContain(t, "输出", te.all(), "现有候选密钥都解不开", "storage_type", "之前部署时 dbtool.env")
}

// 没设任何密钥：服务用公开缺省值，自检也按这个算（与服务一致），缺省加密的值解得开。
func TestCheckSecretsNoKeyEnvUsesServerDefault(t *testing.T) {
	old := mustMirage(t, config.DefaultSecretKey())
	st := newMemStore()
	st.secrets = []SecretRow{{Column: ColSettingValue, Ref: "storage_type", Value: old.Encrypt("local")}}
	te := newTestEnv(t, st)
	if code := te.run("check-secrets"); code != ExitOK {
		t.Fatalf("没设密钥时服务用缺省值，自检应一致: %d\n%s", code, te.all())
	}
	mustContain(t, "输出", te.all(), "未设置任何密钥")
}

func TestCheckSecretsErrorsAndUsage(t *testing.T) {
	st := newMemStore()
	st.secretErr = errors.New("table exploded")
	te := newTestEnv(t, st)
	if code := te.run("check-secrets"); code != ExitFailed {
		t.Errorf("读库失败应返回 1: %d", code)
	}
	te.reset()
	if code := te.run("check-secrets", "extra"); code != ExitUsage {
		t.Errorf("位置参数应返回 2: %d", code)
	}
	te.reset()
	delete(te.env, "DATABASE_URL")
	if code := te.run("check-secrets"); code != ExitUsage {
		t.Errorf("缺 DATABASE_URL 应返回 2: %d", code)
	}
	te.reset()
	if code := te.run("check-secrets", "--help"); code != ExitOK || !strings.Contains(te.out.String(), "用法：dbt-admin check-secrets") {
		t.Errorf("--help: %d %q", code, te.out.String())
	}
	te.reset()
	te.run("--help")
	mustContain(t, "总帮助", te.out.String(), "check-secrets")
}

// mirage 在密钥错误时约 0.4% 的概率碰巧通过填充校验：解出来的不是合法文本就不算“解开”。
func TestSecretKeyCandOpensRejectsGarbage(t *testing.T) {
	m := mustMirage(t, testNewKey)
	c := secretKeyCand{label: "x", m: m}
	if !c.opens(m.Encrypt("hello 世界")) {
		t.Error("正常密文应解得开")
	}
	if !c.opens(m.Encrypt("")) {
		t.Error("空串的密文也应解得开")
	}
	// 含控制字符的“明文”不算像样
	if c.opens(m.Encrypt("bad\x00value")) {
		t.Error("含控制字符的解密结果应判为解不开")
	}
	if c.opens("not-a-ciphertext") {
		t.Error("不是密文应判为解不开")
	}
}

// 用户的真实情况：SECRET_KEY 是公开缺省值（从旧 .env 原样带过来），MIRAGE_SECRET_KEY 是新生成的随机值。
// 旧数据是用缺省值加密的，所以要解开只能把缺省值放到 MIRAGE_SECRET_KEY 上、把 SECRET_KEY 换成随机值。
func TestCheckSecretsPublicSecretKeyWithRandomMirageKey(t *testing.T) {
	old := mustMirage(t, config.DefaultSecretKey())
	st := newMemStore()
	st.secrets = []SecretRow{{Column: ColInstancePassword, Ref: "prod-mysql-01", Value: old.Encrypt("pw")}}
	te := newTestEnv(t, st)
	te.env["SECRET_KEY"] = config.DefaultSecretKey()
	te.env["MIRAGE_SECRET_KEY"] = testNewKey
	te.env["MIRAGE_CIPHER_MODE"] = "ECB"
	if code := te.run("check-secrets"); code != ExitFailed {
		t.Fatalf("%d\n%s", code, te.all())
	}
	out := te.all()
	mustContain(t, "输出", out, "与公开缺省值相同", "prod-mysql-01", "SECRET_KEY 是公开的缺省值", "伪造会话", "gen-secrets",
		"internal/config/config.go")
	if strings.Contains(out, config.DefaultSecretKey()) || strings.Contains(out, testNewKey) {
		t.Fatal("输出里不能出现密钥值")
	}
	// 数据都解得开、但 SECRET_KEY 仍是公开值：返回 0，只给出安全提醒
	cur := mustMirage(t, testNewKey)
	st.secrets = []SecretRow{{Column: ColInstancePassword, Ref: "a", Value: cur.Encrypt("pw")}}
	te.reset()
	if code := te.run("check-secrets"); code != ExitOK {
		t.Fatalf("%d\n%s", code, te.all())
	}
	mustContain(t, "输出", te.all(), "SECRET_KEY 是公开的缺省值", "密钥与数据一致")
	// SECRET_KEY 已换成随机值：没有这条提醒
	te.env["SECRET_KEY"] = testOldKey
	te.reset()
	te.run("check-secrets")
	if strings.Contains(te.all(), "公开的缺省值") {
		t.Errorf("SECRET_KEY 不是公开值时不应提醒: %s", te.all())
	}
}
