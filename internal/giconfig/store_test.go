// @author: kevinkelly
// @license: Apache Licence
// @file: store_test.go
// @time: 2026/10/28

package giconfig

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"dbtool/internal/crypto"
)

// 这里只测不依赖数据库的部分：加解密与序列化。SQL 本身（upsert、行锁、版本号分配）需要在真库上验证。

func testMirage(t *testing.T) *crypto.Mirage {
	t.Helper()
	m, err := crypto.NewMirage("unit-test-secret-key-unit-test-secret-key", "ECB", "")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// 旧版本（支持 manual / systemd / docker / command / ssh 五种重启方式）写进 dbt_gi_setting.deploy 的 JSON，SSH 密码与私钥是 mirage 密文。
const legacySSHDeploy = `{"mode":"ssh","unit":"gi","container":"old-box","use_sudo":true,"command":"/opt/gi/restart.sh","restart_timeout_sec":90,
"health_timeout_sec":45,"auto_rollback":false,"wait_tasks":false,"wait_idle_max_sec":300,
"ssh":{"host":"10.0.0.5","port":2222,"user":"ops","auth_type":"password","host_key_policy":"pin","fingerprint":"SHA256:xyz","command":"sudo systemctl restart gi"},
"ssh_password_enc":"CIPHERTEXT-PASSWORD","ssh_private_key_enc":"CIPHERTEXT-KEY"}`

func TestParseDeployLegacy(t *testing.T) {
	// mode=ssh：能读，不报错；容器名视为未设置（残留的 container / use_sudo 不沿用）；其余通用设置保留
	d, err := parseDeploy(legacySSHDeploy)
	if err != nil {
		t.Fatal(err)
	}
	want := Deploy{RestartTimeoutSec: 90, HealthTimeoutSec: 45, AutoRollback: false, WaitTasks: false, WaitIdleMaxSec: 300}
	if d != want {
		t.Fatalf("%+v != %+v", d, want)
	}
	if _, err := BuildArgv(d); !errors.Is(err, ErrNoContainer) {
		t.Fatalf("旧的 ssh 数据应视为未配置：%v", err)
	}
	// 其它旧方式同理：manual / systemd / command
	for _, raw := range []string{
		`{"mode":"manual"}`,
		`{"mode":"systemd","unit":"goinception","use_sudo":true}`,
		`{"mode":"command","command":"/opt/gi/restart.sh --fast"}`,
		`{"mode":"systemd","unit":"gi","container":"leftover","use_sudo":true,"health_timeout_sec":30}`,
	} {
		d, err := parseDeploy(raw)
		if err != nil || d.Container != "" || d.UseSudo {
			t.Errorf("%s → %+v %v", raw, d, err)
		}
		if _, err := BuildArgv(d); !errors.Is(err, ErrNoContainer) {
			t.Errorf("%s：应视为未配置：%v", raw, err)
		}
	}
	if d, _ := parseDeploy(`{"mode":"systemd","unit":"gi","container":"leftover","health_timeout_sec":30}`); d.HealthTimeoutSec != 30 || d.RestartTimeoutSec != 60 {
		t.Fatalf("通用设置应保留，缺失的用默认值补齐：%+v", d)
	}
	// mode=docker：保留容器名与 use_sudo
	d, err = parseDeploy(`{"mode":"docker","unit":"ignored","container":" goinception ","use_sudo":true,"auto_rollback":false,"command":"x","ssh":{"host":"h"}}`)
	if err != nil || d.Container != "goinception" || !d.UseSudo || d.AutoRollback || d.HealthTimeoutSec != 60 {
		t.Fatalf("%+v %v", d, err)
	}
	if argv, err := BuildArgv(d); err != nil || strings.Join(argv, " ") != "sudo -n docker restart goinception" {
		t.Fatalf("%v %v", argv, err)
	}
	// 新版本写入的 JSON（没有 mode 字段）：原样读回
	d, err = parseDeploy(`{"container":"gi","use_sudo":true,"restart_timeout_sec":30,"health_timeout_sec":20,"auto_rollback":true,"wait_tasks":true,"wait_idle_max_sec":0}`)
	if err != nil || d.Container != "gi" || !d.UseSudo || d.RestartTimeoutSec != 30 || d.WaitIdleMaxSec != 0 {
		t.Fatalf("%+v %v", d, err)
	}
	// 空对象：全部默认值（容器名未设置）
	if d, err := parseDeploy(`{}`); err != nil || d != DefaultDeploy() {
		t.Fatalf("%+v %v", d, err)
	}
	if _, err := parseDeploy("{bad"); err == nil {
		t.Fatal("坏 JSON 应报错")
	}
}

func TestEncodeDeployRewritesWholeJSON(t *testing.T) {
	// 读旧数据 → 改容器名 → 保存：新 JSON 里不再有任何旧字段，SSH 密文也不见了
	d, err := parseDeploy(legacySSHDeploy)
	if err != nil {
		t.Fatal(err)
	}
	d.Container = "goinception"
	raw, err := encodeDeploy(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"mode", "unit", "command", "ssh", "fingerprint", "CIPHERTEXT", "10.0.0.5", "_enc", "auth_type", "host_key_policy"} {
		if strings.Contains(raw, gone) {
			t.Fatalf("新 JSON 不应含 %q：%s", gone, raw)
		}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got := strings.Join(keys, ","); got != "auto_rollback,container,health_timeout_sec,restart_timeout_sec,use_sudo,wait_idle_max_sec,wait_tasks" {
		t.Fatalf("字段集合不对：%s", got)
	}
	// 往返一致
	back, err := parseDeploy(raw)
	if err != nil || back != d {
		t.Fatalf("%+v != %+v (%v)", back, d, err)
	}
	// 默认值（未设置容器名）也能往返，且仍是“未设置”
	raw, _ = encodeDeploy(DefaultDeploy())
	if back, err := parseDeploy(raw); err != nil || back != DefaultDeploy() || back.Container != "" {
		t.Fatalf("%s → %+v %v", raw, back, err)
	}
}

func TestStoreVersionContentRoundTrip(t *testing.T) {
	s := &store{m: testMirage(t)}
	enc := s.m.Encrypt(baseConf)
	if strings.Contains(enc, "s3cret") || strings.Contains(enc, "[inc]") {
		t.Fatal("密文里不应有明文")
	}
	row := versionRow{ID: 5, Version: 3, Content: enc, ContentSHA256: sha256hex([]byte(baseConf)), Status: StatusApplied, CreatedAt: time.Now()}
	v := s.toVersion(row, true)
	if v.Content != baseConf || v.Version != 3 || v.ID != 5 {
		t.Fatalf("%+v", v)
	}
	if v := s.toVersion(row, false); v.Content != "" {
		t.Fatal("列表不带内容")
	}
	// 大文件（接近上限）也能往返
	big := "[inc]\n# " + strings.Repeat("中文", 100000) + "\n"
	if got := s.m.Decrypt(s.m.Encrypt(big)); got != big {
		t.Fatal("大内容往返失败")
	}
}
