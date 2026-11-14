// @author: kevinkelly
// @license: Apache Licence
// @file: settingscmd_test.go
// @time: 2026/10/28

package dbtadmin

import (
	"context"
	"strings"
	"testing"
	"time"

	"dbtool/internal/crypto"
	"dbtool/internal/settings"
)

const settingsTestKey = "0123456789abcdef0123456789abcdef"

func (m *memStore) SettingRows(context.Context) (map[string]settings.Row, bool, error) {
	if m.settingRows == nil {
		return nil, false, nil
	}
	out := make(map[string]settings.Row, len(m.settingRows))
	for k, v := range m.settingRows {
		out[k] = v
	}
	return out, true, nil
}

func (t *memTx) ApplySettings(ws []settings.Write, hs []settings.History) error {
	m := t.m
	if m.settingRows == nil {
		m.settingRows = map[string]settings.Row{}
	}
	for _, w := range ws {
		cur, exists := m.settingRows[w.Key]
		switch {
		case w.Delete:
			delete(m.settingRows, w.Key)
		case w.SkipExisting && exists:
		default:
			m.settingRows[w.Key] = settings.Row{Key: w.Key, Value: w.Value, Encrypted: w.Encrypted, Version: cur.Version + 1, UpdatedBy: w.By, UpdatedAt: time.Now()}
		}
	}
	m.settingHist = append(m.settingHist, hs...)
	return nil
}

func settingsEnv(t *testing.T) (*testEnv, *memStore) {
	t.Helper()
	st := newMemStore()
	st.tables[tblAuditLog] = true
	st.settingRows = map[string]settings.Row{}
	te := newTestEnv(t, st)
	te.env["SECRET_KEY"] = settingsTestKey
	te.env["MIRAGE_SECRET_KEY"] = settingsTestKey
	return te, st
}

func TestSettingsListAndGet(t *testing.T) {
	te, st := settingsEnv(t)
	st.settingRows[settings.KeyQueryMaxExecSeconds] = settings.Row{Key: settings.KeyQueryMaxExecSeconds, Value: "30", Version: 2, UpdatedBy: "root", UpdatedAt: time.Now()}
	if code := te.run("settings", "list", "--group", "query"); code != ExitOK {
		t.Fatalf("code=%d\n%s", code, te.all())
	}
	out := te.all()
	mustContain(t, "list", out, "[query] 在线查询与脱敏", settings.KeyQueryMaxExecSeconds, "30", settings.KeyQueryMaskEnabled)
	mustNotContain(t, "list", out, settings.KeySecurityLoginLockAttempts)

	te.reset()
	if code := te.run("settings", "list", "--modified"); code != ExitOK {
		t.Fatalf("code=%d\n%s", code, te.all())
	}
	mustContain(t, "modified", te.all(), settings.KeyQueryMaxExecSeconds, "共 1 项")

	te.reset()
	if code := te.run("settings", "get", "--key", settings.KeyQueryMaxExecSeconds); code != ExitOK {
		t.Fatalf("code=%d\n%s", code, te.all())
	}
	mustContain(t, "get", te.all(), "当前值：30", "默认值：60", "第 2 版")
	te.reset()
	if code := te.run("settings", "get", "--key", "no.such"); code != ExitUsage {
		t.Errorf("未知键应返回用法错误: %d", code)
	}
}

func TestSettingsSetDryRunThenApply(t *testing.T) {
	te, st := settingsEnv(t)
	key := settings.KeyQueryMaxExecSeconds
	if code := te.run("settings", "set", "--key", key, "--value", "45"); code != ExitOK {
		t.Fatalf("演练: %d\n%s", code, te.all())
	}
	mustContain(t, "演练", te.all(), "只演练", "没有写任何东西")
	if len(st.settingRows) != 0 || st.txCount != 0 {
		t.Fatal("演练不应写库")
	}
	te.reset()
	if code := te.run("settings", "set", "--key", key, "--value", "45", "--apply"); code != ExitUsage {
		t.Fatalf("非终端又没有 --yes 应拒绝: %d\n%s", code, te.all())
	}
	te.reset()
	if code := te.run("settings", "set", "--key", key, "--value", "45", "--apply", "--yes"); code != ExitOK {
		t.Fatalf("写入: %d\n%s", code, te.all())
	}
	if r := st.settingRows[key]; r.Value != "45" || r.Encrypted || r.UpdatedBy != "dbt-admin(alice)" {
		t.Errorf("写入结果: %+v", r)
	}
	if len(st.audits) != 1 || !strings.Contains(st.audits[0].Action, "修改系统设置") || !strings.Contains(st.audits[0].ExtraInfo, key) {
		t.Errorf("审计: %+v", st.audits)
	}
	te.reset()
	if code := te.run("settings", "set", "--key", key, "--value", "0", "--apply", "--yes"); code != ExitUsage {
		t.Errorf("越界的值应拒绝: %d\n%s", code, te.all())
	}
	te.reset()
	if code := te.run("settings", "reset", "--key", key, "--apply", "--yes"); code != ExitOK {
		t.Fatalf("恢复默认: %d\n%s", code, te.all())
	}
	if _, ok := st.settingRows[key]; ok {
		t.Error("恢复默认应删除行")
	}
}

func TestSettingsSecretsOnlyFromEnvAndEncrypted(t *testing.T) {
	te, st := settingsEnv(t)
	key := settings.KeyGoInceptionPassword
	if code := te.run("settings", "set", "--key", key, "--value", "p@ss", "--apply", "--yes"); code != ExitUsage {
		t.Fatalf("敏感项不接受 --value: %d\n%s", code, te.all())
	}
	te.reset()
	te.env[SettingValueEnv] = "S3cret-Value"
	if code := te.run("settings", "set", "--key", key, "--apply", "--yes"); code != ExitOK {
		t.Fatalf("code=%d\n%s", code, te.all())
	}
	mustNotContain(t, "输出", te.all(), "S3cret-Value")
	r := st.settingRows[key]
	m, _ := crypto.NewMirage(settingsTestKey, "", "1234567890abcdef")
	if !r.Encrypted || r.Value == "S3cret-Value" || m.Decrypt(r.Value) != "S3cret-Value" {
		t.Errorf("敏感值应加密保存: %+v", r)
	}
	for _, a := range st.audits {
		mustNotContain(t, "审计", a.ExtraInfo, "S3cret-Value")
	}
	for _, h := range st.settingHist {
		if h.New != "" || h.Old != "" {
			t.Errorf("敏感项的变更记录不能带值: %+v", h)
		}
	}
	te.reset()
	if code := te.run("settings", "list", "--group", "goinception"); code != ExitOK {
		t.Fatal(code)
	}
	mustContain(t, "list", te.all(), "（已设置）")
	mustNotContain(t, "list", te.all(), "S3cret-Value", r.Value)
}

func TestSettingsHiddenNeedsForce(t *testing.T) {
	te, st := settingsEnv(t)
	key := settings.KeySecurityLocalLastLogin
	if code := te.run("settings", "set", "--key", key, "--value", "2026-01-01 10:00:00", "--apply", "--yes"); code != ExitUsage {
		t.Fatalf("内部状态项没有 --force 应拒绝: %d\n%s", code, te.all())
	}
	te.reset()
	if code := te.run("settings", "set", "--key", key, "--value", "2026-01-01 10:00:00", "--force", "--apply", "--yes"); code != ExitOK {
		t.Fatalf("code=%d\n%s", code, te.all())
	}
	if st.settingRows[key].Value != "2026-01-01 10:00:00" {
		t.Errorf("%+v", st.settingRows[key])
	}
}

// SOAR 程序路径现在页面和命令行都能改，但写法要合格（绝对路径、文件名 soar）。
func TestSettingsSoarPathValidated(t *testing.T) {
	te, st := settingsEnv(t)
	key := settings.KeyOptimizerSoarPath
	if code := te.run("settings", "set", "--key", key, "--value", "/bin/sh", "--apply", "--yes"); code != ExitUsage {
		t.Fatalf("文件名不是 soar 应拒绝: %d\n%s", code, te.all())
	}
	te.reset()
	if code := te.run("settings", "set", "--key", key, "--value", "/opt/soar", "--apply", "--yes"); code != ExitOK {
		t.Fatalf("code=%d\n%s", code, te.all())
	}
	if st.settingRows[key].Value != "/opt/soar" {
		t.Errorf("%+v", st.settingRows[key])
	}
	mustContain(t, "输出", te.all(), "高危项")
}

func TestSettingsTableMissing(t *testing.T) {
	st := newMemStore()
	te := newTestEnv(t, st)
	if code := te.run("settings", "list"); code != ExitFailed {
		t.Errorf("没有 dbt_setting 表应失败: %d", code)
	}
	mustContain(t, "输出", te.all(), "260_setting.sql")
}
