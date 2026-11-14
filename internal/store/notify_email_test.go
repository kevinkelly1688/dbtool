// @author: kevinkelly
// @license: Apache Licence
// @file: notify_email_test.go
// @time: 2026/10/28

package store

import (
	"database/sql/driver"
	"strings"
	"testing"
	"time"

	"dbtool/internal/notify"
)

var emailCols = []string{"channel_id", "config", "update_time"}

const emailCfgJSON = `{"host":"smtp.example.com","port":587,"encryption":"starttls","skip_verify":false,"username":"mailer","allow_plain_auth":false,"from_addr":"dba@example.com","from_name":"DBA","to":["ops@example.com"],"cc":[],"subject_prefix":"[dbtool]"}`

func TestEmailChannelsLoadConfig(t *testing.T) {
	d := &fakeDB{tables: map[string]bool{"dbt_notify_channel": true, "dbt_notify_email": true, "resource_group": true}}
	d.results = []fakeResult{
		{match: "FROM `dbt_notify_email`", cols: emailCols, rows: [][]driver.Value{{int64(1), emailCfgJSON, time.Now()}}},
		{match: "FROM `dbt_notify_channel`", cols: chanCols, rows: [][]driver.Value{
			chanRow(1, 0, "邮件", "email", "", 1),
			chanRow(2, 0, "群", "slack", "", 1),
		}},
	}
	s := newNotifyStore(t, d)
	chans, err := s.Channels(1)
	if err != nil || len(chans) != 2 {
		t.Fatalf("%+v %v", chans, err)
	}
	if c := chans[0]; c.Type != notify.TypeEmail || c.Email == nil || c.Email.Host != "smtp.example.com" || c.Email.Port != 587 || c.Email.FromAddr != "dba@example.com" || c.Email.SkipVerify {
		t.Errorf("邮件渠道应带出 SMTP 配置（证书校验默认开启）: %+v %+v", c, c.Email)
	}
	if chans[1].Email != nil {
		t.Errorf("机器人渠道没有邮件配置: %+v", chans[1].Email)
	}
	views, err := s.ListChannelViews()
	if err != nil || len(views) != 2 {
		t.Fatalf("%+v %v", views, err)
	}
	if v := views[0]; v.Email == nil || v.WebhookHint != "smtp.example.com:587 · STARTTLS" || v.Email.Username != "mailer" {
		t.Errorf("列表展示 SMTP 主机 / 端口 / 加密方式: %+v", v)
	}
}

func TestEmailChannelsSkipConfigQueryWhenNoEmailChannel(t *testing.T) {
	d := &fakeDB{tables: map[string]bool{"dbt_notify_channel": true, "dbt_notify_email": true, "resource_group": true}}
	d.results = []fakeResult{{match: "FROM `dbt_notify_channel`", cols: chanCols, rows: [][]driver.Value{chanRow(1, 1, "群", "slack", "", 1)}}}
	s := newNotifyStore(t, d)
	if _, err := s.Channels(1); err != nil {
		t.Fatal(err)
	}
	if d.has("FROM `dbt_notify_email`") {
		t.Error("没有邮件渠道时不应读取邮件配置表")
	}
}

func TestSaveEmailChannelEncryptsPasswordAndStoresConfig(t *testing.T) {
	d := &fakeDB{tables: map[string]bool{"dbt_notify_channel": true, "dbt_notify_email": true}}
	s := newNotifyStore(t, d)
	cfg := &notify.EmailConfig{Host: "smtp.example.com", Port: 465, Encryption: notify.EncSSL, Username: "mailer", FromAddr: "dba@example.com", To: []string{"ops@example.com"}}
	id, err := s.SaveChannel(0, notify.ChannelInput{Name: "邮件", Type: notify.TypeEmail, Secret: "Sm7p-pass", Enabled: true, Email: cfg, Operator: "root"})
	if err != nil || id == 0 {
		t.Fatalf("%d %v", id, err)
	}
	var chanArgs, emailArgs []driver.Value
	for q, a := range d.args {
		switch {
		case strings.Contains(q, "INSERT INTO `dbt_notify_channel` "):
			chanArgs = a
		case strings.Contains(q, "dbt_notify_email"):
			emailArgs = a
		}
	}
	if chanArgs == nil || emailArgs == nil {
		t.Fatalf("应同时写渠道表与邮件配置表: %v", d.logs)
	}
	joined := func(args []driver.Value) string {
		var b strings.Builder
		for _, a := range args {
			b.WriteString(strings.TrimSpace(strings.ReplaceAll(strings.TrimSpace(toStr(a)), "\n", " ")) + "|")
		}
		return b.String()
	}
	if strings.Contains(joined(chanArgs), "Sm7p-pass") || strings.Contains(joined(emailArgs), "Sm7p-pass") {
		t.Errorf("SMTP 密码必须加密后入库，不能出现明文: %s / %s", joined(chanArgs), joined(emailArgs))
	}
	if !strings.Contains(joined(emailArgs), `"host":"smtp.example.com"`) || !strings.Contains(joined(emailArgs), `"from_addr":"dba@example.com"`) {
		t.Errorf("邮件配置表应保存 SMTP 配置 JSON: %s", joined(emailArgs))
	}
	if s.Mirage.Decrypt(s.Mirage.Encrypt("Sm7p-pass")) != "Sm7p-pass" {
		t.Error("加密往返")
	}
	// 密码是密文，且与签名密钥使用同一个加密函数
	found := false
	for _, a := range chanArgs {
		if str, ok := a.(string); ok && str != "" && s.Mirage.Decrypt(str) == "Sm7p-pass" {
			found = true
		}
	}
	if !found {
		t.Errorf("secret 列应是 SMTP 密码的密文: %v", chanArgs)
	}
}

func TestSaveEmailChannelNeedsTable(t *testing.T) {
	d := &fakeDB{tables: map[string]bool{"dbt_notify_channel": true}}
	s := newNotifyStore(t, d)
	_, err := s.SaveChannel(0, notify.ChannelInput{Name: "邮件", Type: notify.TypeEmail, Enabled: true,
		Email: &notify.EmailConfig{Host: "h", Port: 25, Encryption: notify.EncNone, FromAddr: "a@example.com"}})
	if err == nil || !strings.Contains(err.Error(), "210_notify_email.sql") {
		t.Fatalf("没有 210 表应提示先执行脚本: %v", err)
	}
	if d.has("INSERT INTO `dbt_notify_channel` ") {
		t.Error("失败时不应写渠道表")
	}
	if s.NotifyEmailReady() {
		t.Error("表不存在时 NotifyEmailReady 应为 false")
	}
}

func TestResourceGroupMembers(t *testing.T) {
	d := &fakeDB{tables: map[string]bool{}}
	d.results = []fakeResult{{match: "FROM dbt_sql_users u", cols: []string{"id", "username", "display", "email", "is_active"},
		rows: [][]driver.Value{{int64(1), "alice", "爱丽丝", " alice@example.com ", int64(1)}, {int64(2), "bob", "鲍勃", "", int64(1)}}}}
	s := newNotifyStore(t, d)
	ps, err := s.ResourceGroupMembers(3)
	if err != nil || len(ps) != 2 {
		t.Fatalf("%+v %v", ps, err)
	}
	if ps[0].Email != "alice@example.com" || ps[1].Email != "" {
		t.Errorf("邮箱应取自 dbt_sql_users.email 并去掉空白: %+v", ps)
	}
	if !d.has("sql_users_resource_group") || !d.has("is_active = 1") {
		t.Errorf("应按资源组取启用用户: %v", d.logs)
	}
}

func toStr(v driver.Value) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	}
	return ""
}
