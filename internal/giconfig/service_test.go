// @author: kevinkelly
// @license: Apache Licence
// @file: service_test.go
// @time: 2026/10/28

package giconfig

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var bg = context.Background()

// edit 在“页面上看到的（已脱敏的）当前内容”上做替换，模拟用户在编辑框里修改。
func (e *env) edit(old, new string) string {
	e.t.Helper()
	ed := e.overview().File.Editor
	if !strings.Contains(ed, old) {
		e.t.Fatalf("编辑内容里没有 %q：\n%s", old, ed)
	}
	return strings.Replace(ed, old, new, 1)
}

func (e *env) base() int { return e.overview().LatestNum }

// ---- 连接信息 ----

func TestSaveConnKeepsPasswordWhenBlank(t *testing.T) {
	e := newEnv(t)
	err := e.svc.SaveConn(e.actor, ConnInput{Target: "main", Host: " 10.1.1.1 ", Port: "4001", User: "inc"})
	if err != nil {
		t.Fatal(err)
	}
	if e.repo.cfg[cfgHost] != "10.1.1.1" || e.repo.cfg[cfgPort] != "4001" || e.repo.cfg[cfgUser] != "inc" || e.repo.cfg[cfgPass] != "pw-main" {
		t.Fatalf("%v", e.repo.cfg)
	}
	if err := e.svc.SaveConn(e.actor, ConnInput{Host: "10.1.1.1", Port: "4001", User: "inc", Password: "new-pw"}); err != nil {
		t.Fatal(err)
	}
	if e.repo.cfg[cfgPass] != "new-pw" {
		t.Fatal("填了新密码应更新")
	}
	if err := e.svc.SaveConn(e.actor, ConnInput{Host: "10.1.1.1", Port: "4001", User: "inc", ClearPassword: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.repo.ConfigValue(cfgPass); ok {
		t.Fatal("ClearPassword 应清空密码")
	}
	if a := e.repo.auditText(); strings.Contains(a, "new-pw") || strings.Contains(a, "pw-main") || !strings.Contains(a, "密码已修改") {
		t.Fatalf("审计里不能有密码：%s", a)
	}
}

func TestSaveConnBackupUsesOwnKeys(t *testing.T) {
	e := newEnv(t)
	if err := e.svc.SaveConn(e.actor, ConnInput{Target: "backup", Host: "10.20.9.31", Port: "3306", User: "inc_backup", Password: "bk"}); err != nil {
		t.Fatal(err)
	}
	if e.repo.cfg[cfgBkHost] != "10.20.9.31" || e.repo.cfg[cfgBkPass] != "bk" || e.repo.cfg[cfgHost] != "10.20.9.30" {
		t.Fatalf("%v", e.repo.cfg)
	}
}

func TestSaveConnValidation(t *testing.T) {
	e := newEnv(t)
	bad := []ConnInput{
		{Host: "10.0.0.1", Port: "0"}, {Host: "10.0.0.1", Port: "70000"}, {Host: "10.0.0.1", Port: "abc"},
		{Host: "bad host"}, {Host: "a;b"}, {Host: "10.0.0.1", User: "x\ny"}, {Host: "10.0.0.1", Password: strings.Repeat("p", 201)},
		{Target: "other", Host: "10.0.0.1"},
	}
	for _, in := range bad {
		mustErr(t, e.svc.SaveConn(e.actor, in), 400, "bad_request")
	}
	if e.repo.cfg[cfgHost] != "10.20.9.30" {
		t.Fatal("校验失败不能写入")
	}
	// 清空地址是允许的（表示不使用 goInception）
	if err := e.svc.SaveConn(e.actor, ConnInput{Host: "", Port: "", User: ""}); err != nil {
		t.Fatal(err)
	}
}

func TestTestConn(t *testing.T) {
	e := newEnv(t)
	// 地址账号没变，密码留空：沿用已保存的密码
	r, err := e.svc.TestConn(bg, ConnInput{Host: "10.20.9.30", Port: "4000", User: "inception"})
	if err != nil || !r.OK {
		t.Fatalf("%+v %v", r, err)
	}
	if e.pr.lastConn.Password != "pw-main" {
		t.Fatal("应沿用已保存的密码")
	}
	if !strings.Contains(strings.Join(r.Lines, "\n"), "v1.2.7") {
		t.Fatalf("应显示版本：%v", r.Lines)
	}
	// 改了地址但没填密码：不能把已保存的密码发给新地址
	e.pr.lastConn = ConnInfo{}
	r, _ = e.svc.TestConn(bg, ConnInput{Host: "evil.example.com", Port: "4000", User: "inception"})
	if r.OK || e.pr.lastConn.Host != "" || !strings.Contains(r.Lines[0], "重新输入密码") {
		t.Fatalf("改地址后必须重新输入密码：%+v", r)
	}
	// 填了新密码就用新密码
	r, _ = e.svc.TestConn(bg, ConnInput{Host: "evil.example.com", Port: "4000", User: "inception", Password: "typed"})
	if !r.OK || e.pr.lastConn.Password != "typed" {
		t.Fatalf("%+v", r)
	}
	// 连接失败：200 + ok=false + 原因
	e.pr.pingErr = errors.New("dial tcp: i/o timeout")
	r, err = e.svc.TestConn(bg, ConnInput{Host: "10.20.9.30", Port: "4000", User: "inception"})
	if err != nil || r.OK || !strings.Contains(r.Lines[0], "i/o timeout") {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := e.svc.TestConn(bg, ConnInput{Host: ""}); err == nil {
		t.Fatal("没有地址应报错")
	}
}

func TestTestConnBackup(t *testing.T) {
	e := newEnv(t)
	yes := true
	e.pr.backup = BackupResult{LatencyMS: 3, Version: "8.0.36", CanWrite: &yes}
	r, err := e.svc.TestConn(bg, ConnInput{Target: "backup", Host: "10.20.9.31", Port: "3306", User: "u", Password: "p"})
	if err != nil || !r.OK || !strings.Contains(strings.Join(r.Lines, "\n"), "有写权限") {
		t.Fatalf("%+v %v", r, err)
	}
	e.pr.backupErr = errors.New("access denied")
	r, _ = e.svc.TestConn(bg, ConnInput{Target: "backup", Host: "10.20.9.31", Port: "3306", User: "u", Password: "p"})
	if r.OK || !strings.Contains(r.Lines[0], "access denied") {
		t.Fatalf("%+v", r)
	}
}

// ---- 部署设置 ----

func TestSaveDeployValidation(t *testing.T) {
	e := newEnv(t)
	good := func() DeployInput {
		d := DefaultDeploy()
		d.Container = "goinception"
		return DeployInput{Deploy: d, ConfigPath: testPath}
	}
	if _, err := e.svc.SaveDeploy(e.actor, good()); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(in *DeployInput){
		"容器名带空格":    func(in *DeployInput) { in.Container = "a b" },
		"容器名带斜杠":    func(in *DeployInput) { in.Container = "a/b" },
		"容器名以-开头":   func(in *DeployInput) { in.Container = "-rf" },
		"容器名带分号":    func(in *DeployInput) { in.Container = "x;rm" },
		"容器名命令替换":   func(in *DeployInput) { in.Container = "$(reboot)" },
		"容器名换行":     func(in *DeployInput) { in.Container = "a\nb" },
		"容器名过长":     func(in *DeployInput) { in.Container = strings.Repeat("a", 129) },
		"重启超时过小":    func(in *DeployInput) { in.RestartTimeoutSec = 1 },
		"健康检查超时过大":  func(in *DeployInput) { in.HealthTimeoutSec = 9999 },
		"等待任务时间为负":  func(in *DeployInput) { in.WaitIdleMaxSec = -1 },
		"路径是相对路径":   func(in *DeployInput) { in.ConfigPath = "config.toml" },
		"路径不是 toml": func(in *DeployInput) { in.ConfigPath = "/etc/passwd" },
		"路径含控制字符":   func(in *DeployInput) { in.ConfigPath = "/etc/a\n.toml" },
	}
	for name, mut := range cases {
		in := good()
		mut(&in)
		_, err := e.svc.SaveDeploy(e.actor, in)
		if err == nil {
			t.Errorf("%s：应拒绝", name)
			continue
		}
		mustErr(t, err, 400, "bad_request")
	}
	if d, _, _ := e.repo.LoadDeploy(); d.Container != "goinception" {
		t.Fatalf("校验失败不能改动已保存的设置：%+v", d)
	}
	// 容器名前后的空白会被去掉；留空是允许的（表示尚未设置）
	in := good()
	in.Container = "  gi-1  "
	v, err := e.svc.SaveDeploy(e.actor, in)
	if err != nil || v.Container != "gi-1" || !v.Configured || v.Preview != "docker restart gi-1" {
		t.Fatalf("%v %+v", err, v)
	}
	in.Container = ""
	v, err = e.svc.SaveDeploy(e.actor, in)
	if err != nil || v.Container != "" || v.Configured || v.Preview != "" || v.PreviewError != "" {
		t.Fatalf("留空应能保存，并视为未设置：%v %+v", err, v)
	}
	// 需要数据库表
	e.repo.noTables = true
	_, err = e.svc.SaveDeploy(e.actor, good())
	mustErr(t, err, 409, "table_missing")
}

func TestSaveDeployViewAndAudit(t *testing.T) {
	e := newEnv(t)
	in := DeployInput{Deploy: DefaultDeploy(), ConfigPath: testPath}
	in.Container, in.UseSudo = "goinception", true
	in.HealthTimeoutSec = 30
	v, err := e.svc.SaveDeploy(e.actor, in)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Configured || v.Preview != "sudo -n docker restart goinception" || v.PreviewError != "" || !v.UseSudo || v.HealthTimeoutSec != 30 || v.ConfigPath != testPath {
		t.Fatalf("%+v", v)
	}
	d, ok, _ := e.repo.LoadDeploy()
	if !ok || d.Container != "goinception" || !d.UseSudo || d.HealthTimeoutSec != 30 {
		t.Fatalf("%+v", d)
	}
	a := e.repo.auditText()
	if !strings.Contains(a, actDeploy) || !strings.Contains(a, "重启方式：sudo -n docker restart goinception") || !strings.Contains(a, "健康检查等待 30 秒") {
		t.Fatalf("审计：%s", a)
	}
	// 保存部署设置只落库，不执行任何命令
	if e.rs.count() != 0 {
		t.Fatal("保存部署设置不应执行命令")
	}
	// 容器名没设置时，审计写明未设置
	in.Container = ""
	if _, err := e.svc.SaveDeploy(e.actor, in); err != nil {
		t.Fatal(err)
	}
	if a := e.repo.auditText(); !strings.Contains(a, "重启方式：docker restart（未设置容器名）") {
		t.Fatalf("审计：%s", a)
	}
}

func TestPreviewDeployDoesNotExecute(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		container string
		sudo      bool
		preview   string
		problem   bool
	}{
		{"goinception", false, "docker restart goinception", false},
		{" goinception ", true, "sudo -n docker restart goinception", false},
		{"", false, "", true},
		{"", true, "", true},
		{"a b", false, "", true},
		{"-rf", false, "", true},
		{"x;reboot", true, "", true},
	}
	for _, c := range cases {
		d := DefaultDeploy()
		d.Container, d.UseSudo = c.container, c.sudo
		p, problem := e.svc.PreviewDeploy(d)
		if p != c.preview || (problem != "") != c.problem {
			t.Errorf("%+v → %q / %q", c, p, problem)
		}
	}
	if e.rs.count() != 0 {
		t.Fatalf("预览不能执行命令：%v", e.rs.commands())
	}
}

func TestConfigPathEnvWins(t *testing.T) {
	e := newEnv(t)
	e.svc.env = func(k string) string {
		if k == envFileVar {
			return "/opt/gi/config.toml"
		}
		return ""
	}
	o := e.overview()
	if o.File.Path != "/opt/gi/config.toml" || o.File.Source != "env" || o.Deploy.ConfigPathSource != "env" {
		t.Fatalf("%+v", o.File)
	}
	in := DeployInput{Deploy: DefaultDeploy(), ConfigPath: "/should/be/ignored.toml"}
	if _, err := e.svc.SaveDeploy(e.actor, in); err != nil {
		t.Fatal(err)
	}
	if e.repo.cfg[cfgFile] != testPath {
		t.Fatalf("环境变量生效时不应改 dbt_sql_config：%q", e.repo.cfg[cfgFile])
	}
	e.svc.env = func(string) string { return "relative.toml" }
	if _, err := e.svc.Overview(); err != nil {
		t.Fatal(err) // 环境变量非法时页面仍能打开，只是路径报错
	}
	if _, err := e.svc.Save(bg, e.actor, SaveRequest{Content: "[inc]\n"}); err == nil {
		t.Fatal("环境变量路径非法时保存应报错")
	}
}

func TestCheckDeploy(t *testing.T) {
	e := newEnv(t)
	e.useDocker()
	r, err := e.svc.CheckDeploy(bg)
	if err != nil || !r.OK {
		t.Fatalf("%+v %v", r, err)
	}
	txt := strings.Join(r.Lines, "\n")
	if !strings.Contains(txt, "将执行：docker restart goinception") || !strings.Contains(txt, "可连接") {
		t.Fatal(txt)
	}
	// 勾选 sudo：预览带 sudo -n
	d := DefaultDeploy()
	d.Container, d.UseSudo = "goinception", true
	_ = e.repo.SaveDeploy(d, "root")
	if r, _ = e.svc.CheckDeploy(bg); !r.OK || !strings.Contains(strings.Join(r.Lines, "\n"), "将执行：sudo -n docker restart goinception") {
		t.Fatalf("%+v", r)
	}
	// 配置文件目录不可写
	e.files.checkErr = errors.New("目录不可写")
	r, _ = e.svc.CheckDeploy(bg)
	if r.OK || !strings.Contains(strings.Join(r.Lines, "\n"), "目录不可写") {
		t.Fatalf("%+v", r)
	}
	e.files.checkErr = nil
	// goInception 当前连不上：不算失败，只是提示
	e.pr.setUp(false)
	if r, _ = e.svc.CheckDeploy(bg); !r.OK || !strings.Contains(strings.Join(r.Lines, "\n"), "当前连不上") {
		t.Fatalf("%+v", r)
	}
	e.pr.setUp(true)
	// 没有设置容器名：检查不通过，并说明原因
	_ = e.repo.SaveDeploy(DefaultDeploy(), "root")
	r, _ = e.svc.CheckDeploy(bg)
	if r.OK || !strings.Contains(strings.Join(r.Lines, "\n"), "尚未设置 goInception 的 Docker 容器名") {
		t.Fatalf("%+v", r)
	}
	// 没有 goInception 连接信息：无法做健康检查
	e.useDocker()
	delete(e.repo.cfg, cfgHost)
	if r, _ = e.svc.CheckDeploy(bg); r.OK || !strings.Contains(strings.Join(r.Lines, "\n"), "连接信息") {
		t.Fatalf("%+v", r)
	}
	// 检查权限与连通性是只读的：任何一种情况下都不会执行 docker restart
	if e.rs.count() != 0 {
		t.Fatalf("检查不能执行命令：%v", e.rs.commands())
	}
}

// ---- 概览、保存（不重启） ----

func TestOverviewFresh(t *testing.T) {
	e := newEnv(t)
	e.files.set(testPath, baseConf)
	o := e.overview()
	if !o.TableReady || o.File.Drift.Kind != "unmanaged" || o.LatestNum != 0 || o.Effective != nil {
		t.Fatalf("%+v", o.File.Drift)
	}
	if strings.Contains(o.File.Editor, "s3cret") || !strings.Contains(o.File.Editor, `"********"`) {
		t.Fatalf("编辑内容应已脱敏：\n%s", o.File.Editor)
	}
	if o.Conn.Host != "10.20.9.30" || !o.Conn.HasPassword || o.Backup.HasPassword {
		t.Fatalf("%+v %+v", o.Conn, o.Backup)
	}
	if len(o.Catalog) != 4 || o.Deploy.Container != "" || o.Deploy.Configured || o.Deploy.Preview != "" || o.Deploy.PreviewError != "" || !o.Deploy.AutoRollback {
		t.Fatalf("目录或默认部署设置不对（默认应未设置容器名）：%+v", o.Deploy)
	}
	// 文件不存在：用空白模板
	e.files.files = map[string]string{}
	o = e.overview()
	if o.File.EditorFrom != "空白模板" || o.File.Drift.Kind != "none" {
		t.Fatalf("%+v", o.File)
	}
	// 未配置路径
	delete(e.repo.cfg, cfgFile)
	o = e.overview()
	if o.File.Source != "none" || o.File.Path != "" {
		t.Fatalf("%+v", o.File)
	}
	// 表未创建
	e.repo.noTables = true
	o = e.overview()
	if o.TableReady {
		t.Fatal("应提示表未创建")
	}
	mustErr(t, func() error { _, err := e.svc.Save(bg, e.actor, SaveRequest{}); return err }(), 409, "table_missing")
}

func TestSaveWithoutRestart(t *testing.T) {
	e := newEnv(t)
	e.seed()
	content := e.edit("check_dml_limit = false", "check_dml_limit = true")
	res, err := e.svc.Save(bg, e.actor, SaveRequest{Content: content, BaseVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != 2 || res.Status != StatusDraft || res.Restarting {
		t.Fatalf("%+v", res)
	}
	disk := e.files.get(testPath)
	if !strings.Contains(disk, "check_dml_limit = true") || !strings.Contains(disk, `backup_password = "s3cret"`) {
		t.Fatalf("写入磁盘的内容应还原了密码占位符：\n%s", disk)
	}
	v := e.repo.version(2)
	if v.ChangeSummary != "check_dml_limit: false → true" || v.Operator != "root" || v.ContentSHA256 != sha256hex([]byte(disk)) {
		t.Fatalf("%+v", v)
	}
	o := e.overview()
	if o.Pending == nil || o.Pending.Version != 2 || o.Effective.Version != 1 || o.File.Drift.Kind != "none" {
		t.Fatalf("pending=%+v drift=%+v", o.Pending, o.File.Drift)
	}
	if o.Versions[0].Label != "已保存，待重启生效" || o.Versions[1].Label != "当前生效" {
		t.Fatalf("%+v", o.Versions)
	}
	a := e.repo.auditText()
	if !strings.Contains(a, actSave) || strings.Contains(a, "s3cret") {
		t.Fatalf("审计：%s", a)
	}
	// 草稿不是“当前生效”；此时编辑框默认展示草稿
	if !strings.Contains(o.File.Editor, "check_dml_limit = true") || !strings.Contains(o.File.EditorFrom, "#2") {
		t.Fatalf("%s", o.File.EditorFrom)
	}
}

func TestSaveGuards(t *testing.T) {
	e := newEnv(t)
	e.seed()
	ed := e.overview().File.Editor
	t.Run("乐观并发：版本号过期", func(t *testing.T) {
		_, err := e.svc.Save(bg, e.actor, SaveRequest{Content: strings.Replace(ed, "max_update_rows = 5000", "max_update_rows = 6000", 1), BaseVersion: 0})
		mustErr(t, err, 409, "stale")
	})
	t.Run("没有变化", func(t *testing.T) {
		_, err := e.svc.Save(bg, e.actor, SaveRequest{Content: ed, BaseVersion: 1})
		mustErr(t, err, 400, "bad_request")
	})
	t.Run("语法错误", func(t *testing.T) {
		_, err := e.svc.Save(bg, e.actor, SaveRequest{Content: "[inc\n", BaseVersion: 1})
		ee := mustErr(t, err, 400, "invalid_config")
		if rep, ok := ee.Data.(Report); !ok || rep.Errors != 1 {
			t.Fatalf("%+v", ee.Data)
		}
	})
	t.Run("类型错误", func(t *testing.T) {
		_, err := e.svc.Save(bg, e.actor, SaveRequest{Content: strings.Replace(ed, "check_primary_key = true", `check_primary_key = "yes"`, 1), BaseVersion: 1})
		mustErr(t, err, 400, "invalid_config")
	})
	t.Run("警告需要确认", func(t *testing.T) {
		c := strings.Replace(ed, "[log]", "enable_drop_table = true\n\n[log]", 1)
		_, err := e.svc.Save(bg, e.actor, SaveRequest{Content: c, BaseVersion: 1})
		ee := mustErr(t, err, 422, "warnings")
		if rep, ok := ee.Data.(Report); !ok || rep.Warnings == 0 {
			t.Fatalf("%+v", ee.Data)
		}
		if len(e.repo.versions) != 1 {
			t.Fatal("未确认警告时不能写版本")
		}
		res, err := e.svc.Save(bg, e.actor, SaveRequest{Content: c, BaseVersion: 1, AckWarnings: true})
		if err != nil || len(res.Warnings) == 0 {
			t.Fatalf("%v %+v", err, res)
		}
	})
}

func TestSaveSecrets(t *testing.T) {
	e := newEnv(t)
	e.seed()
	ed := e.overview().File.Editor
	res, err := e.svc.Save(bg, e.actor, SaveRequest{Content: ed, BaseVersion: 1, Secrets: map[string]string{"inc.backup_password": "n3w-pw"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.files.get(testPath), `backup_password = "n3w-pw"`) {
		t.Fatalf("新密码应写入文件：\n%s", e.files.get(testPath))
	}
	if sum := e.repo.version(res.Version).ChangeSummary; strings.Contains(sum, "n3w-pw") || !strings.Contains(sum, "已修改") {
		t.Fatalf("摘要：%q", sum)
	}
	if a := e.repo.auditText(); strings.Contains(a, "n3w-pw") {
		t.Fatalf("审计泄露密码：%s", a)
	}
	// 非密码参数不能走 secrets
	_, err = e.svc.Save(bg, e.actor, SaveRequest{Content: ed, BaseVersion: 2, Secrets: map[string]string{"inc.max_keys": "1"}})
	mustErr(t, err, 400, "bad_request")
	// 占位符在基线里找不到原值：拒绝，而不是把占位符写进文件
	e.files.set(testPath, "[inc]\nlang = \"zh-CN\"\n")
	e.repo.versions = nil
	_, err = e.svc.Save(bg, e.actor, SaveRequest{Content: ed, BaseVersion: 0})
	if err == nil || !strings.Contains(err.Error(), "backup_password") {
		t.Fatalf("%v", err)
	}
}

func TestSaveDriftNeedsAck(t *testing.T) {
	e := newEnv(t)
	e.seed()
	e.files.set(testPath, baseConf+"# 有人手工改过\nmax_keys = 3\n")
	o := e.overview()
	if o.File.Drift.Kind != "modified" || !strings.Contains(o.File.Drift.Message, "#1") {
		t.Fatalf("%+v", o.File.Drift)
	}
	content := strings.Replace(MaskContent(baseConf), "max_update_rows = 5000", "max_update_rows = 6000", 1)
	_, err := e.svc.Save(bg, e.actor, SaveRequest{Content: content, BaseVersion: 1})
	mustErr(t, err, 409, "drift")
	if _, err := e.svc.Save(bg, e.actor, SaveRequest{Content: content, BaseVersion: 1, AckDrift: true}); err != nil {
		t.Fatal(err)
	}
	// 磁盘文件被删：提示 missing，保存可以直接重建
	e.files.files = map[string]string{}
	if d := e.overview().File.Drift; d.Kind != "missing" {
		t.Fatalf("%+v", d)
	}
}

func TestFirstSaveUnmanaged(t *testing.T) {
	e := newEnv(t)
	e.files.set(testPath, baseConf)
	ed := e.overview().File.Editor
	_, err := e.svc.Save(bg, e.actor, SaveRequest{Content: ed, BaseVersion: 0})
	mustErr(t, err, 400, "bad_request") // 与磁盘相同
	res, err := e.svc.Save(bg, e.actor, SaveRequest{Content: strings.Replace(ed, "5000", "6000", 1), BaseVersion: 0})
	if err != nil || res.Version != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	if e.repo.version(1).ChangeSummary != "max_update_rows: 5000 → 6000" {
		t.Fatal(e.repo.version(1).ChangeSummary)
	}
}

func TestSaveNoPathAndWriteFailure(t *testing.T) {
	e := newEnv(t)
	e.seed()
	e.files.failWrite[e.files.nWrites+1] = errors.New("read-only file system")
	_, err := e.svc.Save(bg, e.actor, SaveRequest{Content: e.edit("5000", "6000"), BaseVersion: 1})
	mustErr(t, err, 500, "write_failed")
	if v := e.repo.version(2); v.Status != StatusFailed || !strings.Contains(v.Error, "read-only") {
		t.Fatalf("%+v", v)
	}
	if e.files.get(testPath) != baseConf {
		t.Fatal("写失败不能改动原文件")
	}
	if e.repo.lock != nil {
		t.Fatal("失败后必须释放锁")
	}
	delete(e.repo.cfg, cfgFile)
	_, err = e.svc.Save(bg, e.actor, SaveRequest{Content: "[inc]\n", BaseVersion: 2})
	mustErr(t, err, 400, "no_path")
}

func TestSaveInsertFails(t *testing.T) {
	e := newEnv(t)
	e.seed()
	e.repo.failNext = "insert"
	_, err := e.svc.Save(bg, e.actor, SaveRequest{Content: e.edit("5000", "6000"), BaseVersion: 1})
	mustErr(t, err, 500, "save_failed")
	if e.files.get(testPath) != baseConf || e.repo.lock != nil {
		t.Fatal("版本记录写失败时不能改文件，且要释放锁")
	}
}

// ---- 保存并重启 ----

func TestSaveAndRestartSuccess(t *testing.T) {
	e := newEnv(t)
	e.useDocker()
	e.seed()
	res, err := e.svc.Save(bg, e.actor, SaveRequest{Content: e.edit("5000", "6000"), BaseVersion: 1, Restart: true})
	if err != nil || !res.Restarting {
		t.Fatalf("%v %+v", err, res)
	}
	e.svc.Wait()
	v := e.repo.version(2)
	if v.Status != StatusApplied || v.AppliedAt == nil || v.Error != "" {
		t.Fatalf("%+v", v)
	}
	job := e.svc.JobView()
	if !job.Done || !job.OK || job.Result != "success" || job.Version != 2 {
		t.Fatalf("%+v", job)
	}
	var names []string
	for _, s := range job.Steps {
		names = append(names, s.Name+":"+s.Status)
	}
	if got := strings.Join(names, ","); got != "写入配置文件（版本 #2）:ok,重启 goInception:ok,健康检查:ok" {
		t.Fatalf("步骤：%s", got)
	}
	if got := e.rs.commands(); len(got) != 1 || got[0] != "docker restart goinception" {
		t.Fatalf("应恰好执行一次 docker restart goinception：%v", got)
	}
	if rec := e.svc.lastRestart(); rec == nil || !rec.OK || rec.Version != 2 || rec.Operator != "root" {
		t.Fatalf("%+v", rec)
	}
	o := e.overview()
	if o.Effective.Version != 2 || o.Pending != nil || o.LastResult == nil || o.Job == nil {
		t.Fatalf("%+v", o)
	}
	if a := e.repo.auditText(); !strings.Contains(a, actRestart+" 版本 #2，重启方式 docker restart goinception：成功") {
		t.Fatalf("审计：%s", a)
	}
	if e.repo.lock != nil {
		t.Fatal("流程结束必须释放锁")
	}
}

func TestSaveAndRestartWithSudo(t *testing.T) {
	e := newEnv(t)
	d := DefaultDeploy()
	d.Container, d.UseSudo = "gi_1", true
	_ = e.repo.SaveDeploy(d, "root")
	e.seed()
	if _, err := e.svc.Save(bg, e.actor, SaveRequest{Content: e.edit("5000", "6000"), BaseVersion: 1, Restart: true}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if got := e.rs.commands(); len(got) != 1 || got[0] != "sudo -n docker restart gi_1" {
		t.Fatalf("%v", got)
	}
	if e.rs.argvs[0][0] != "sudo" || len(e.rs.argvs[0]) != 5 {
		t.Fatalf("每一项必须是独立的参数：%q", e.rs.argvs[0])
	}
	if e.repo.version(2).Status != StatusApplied {
		t.Fatal()
	}
}

func TestRestartHealthFailureAutoRollback(t *testing.T) {
	e := newEnv(t)
	e.useDocker()
	e.seed()
	content := strings.Replace(e.overview().File.Editor, "[log]", "BAD_PARAM = 1\n\n[log]", 1)
	if _, err := e.svc.Save(bg, e.actor, SaveRequest{Content: content, BaseVersion: 1, Restart: true, AckWarnings: true}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	v := e.repo.version(2)
	if v.Status != StatusRolledBack || !strings.Contains(v.Error, "上一个生效版本 #1") || !strings.Contains(v.Error, "健康检查失败") {
		t.Fatalf("%+v", v)
	}
	if e.files.get(testPath) != baseConf {
		t.Fatalf("磁盘应恢复到 #1：\n%s", e.files.get(testPath))
	}
	if got := e.rs.commands(); len(got) != 2 || got[0] != "docker restart goinception" || got[1] != "docker restart goinception" {
		t.Fatalf("应重启两次（新配置 + 恢复），实际 %v", got)
	}
	job := e.svc.JobView()
	if job.OK || job.Result != "rolledback" || !strings.Contains(job.Message, "已自动恢复") {
		t.Fatalf("%+v", job)
	}
	if o := e.overview(); o.Effective.Version != 1 || o.Pending != nil {
		t.Fatalf("生效版本仍应是 #1：%+v", o.Effective)
	}
	if !e.pr.up {
		t.Fatal("恢复后 goInception 应可连接")
	}
	if a := e.repo.auditText(); !strings.Contains(a, "已自动恢复旧配置") {
		t.Fatal(a)
	}
}

func TestRestartCommandFailure(t *testing.T) {
	e := newEnv(t)
	e.useDocker()
	e.seed()
	e.rs.failOn[1] = errors.New("exit status 5")
	if _, err := e.svc.Save(bg, e.actor, SaveRequest{Content: e.edit("5000", "6000"), BaseVersion: 1, Restart: true}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	v := e.repo.version(2)
	if v.Status != StatusRolledBack || !strings.Contains(v.Error, "重启命令失败") || !strings.Contains(v.Error, "exit status 5") {
		t.Fatalf("%+v", v)
	}
}

func TestRestartAndRecoveryBothFail(t *testing.T) {
	e := newEnv(t)
	e.useDocker()
	e.seed()
	e.rs.failOn[1] = errors.New("boom")
	e.rs.failOn[2] = errors.New("boom again")
	if _, err := e.svc.Save(bg, e.actor, SaveRequest{Content: e.edit("5000", "6000"), BaseVersion: 1, Restart: true}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	v := e.repo.version(2)
	if v.Status != StatusFailed || !strings.Contains(v.Error, "仍失败") || !strings.Contains(v.Error, "人工处理") {
		t.Fatalf("%+v", v)
	}
	if e.svc.JobView().Result != "failed" {
		t.Fatal()
	}
}

func TestRestartRecoveryWriteFails(t *testing.T) {
	e := newEnv(t)
	e.useDocker()
	e.seed()
	e.rs.failOn[1] = errors.New("boom")
	e.files.failWrite[e.files.nWrites+2] = errors.New("disk full") // 第 1 次是写新配置，第 2 次是恢复
	if _, err := e.svc.Save(bg, e.actor, SaveRequest{Content: e.edit("5000", "6000"), BaseVersion: 1, Restart: true}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if v := e.repo.version(2); v.Status != StatusFailed || !strings.Contains(v.Error, "disk full") {
		t.Fatalf("%+v", v)
	}
}

func TestNoAutoRollbackWhenDisabled(t *testing.T) {
	e := newEnv(t)
	d := DefaultDeploy()
	d.Container, d.AutoRollback = testContainer, false
	_ = e.repo.SaveDeploy(d, "root")
	e.seed()
	e.rs.err = errors.New("fail")
	if _, err := e.svc.Save(bg, e.actor, SaveRequest{Content: e.edit("5000", "6000"), BaseVersion: 1, Restart: true}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	v := e.repo.version(2)
	if v.Status != StatusFailed || !strings.Contains(v.Error, "未开启自动回滚") || e.rs.count() != 1 {
		t.Fatalf("%+v 重启 %d 次", v, e.rs.count())
	}
	if !strings.Contains(e.files.get(testPath), "6000") {
		t.Fatal("未开启自动回滚时磁盘保持新配置")
	}
}

func TestFirstSaveFailureRestoresPreviousDisk(t *testing.T) {
	e := newEnv(t)
	e.useDocker()
	e.files.set(testPath, baseConf) // 还没纳入版本管理
	e.rs.failOn[1] = errors.New("boom")
	if _, err := e.svc.Save(bg, e.actor, SaveRequest{Content: e.edit("5000", "6000"), BaseVersion: 0, Restart: true}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	v := e.repo.version(1)
	if v.Status != StatusRolledBack || !strings.Contains(v.Error, "保存前磁盘上的文件") || e.files.get(testPath) != baseConf {
		t.Fatalf("%+v\n%s", v, e.files.get(testPath))
	}
	// 磁盘上原本没有文件，也没有旧版本：无从恢复
	e2 := newEnv(t)
	e2.useDocker()
	e2.rs.failOn[1] = errors.New("boom")
	if _, err := e2.svc.Save(bg, e2.actor, SaveRequest{Content: "[inc]\nlang = \"zh-CN\"\n", BaseVersion: 0, Restart: true}); err != nil {
		t.Fatal(err)
	}
	e2.svc.Wait()
	if v := e2.repo.version(1); v.Status != StatusFailed || !strings.Contains(v.Error, "没有可恢复") || e2.rs.count() != 1 {
		t.Fatalf("%+v", v)
	}
}

const notSetMsg = "尚未设置 goInception 的 Docker 容器名，请先到「部署与重启」里填写"

func TestRestartActionsRejectedWithoutContainer(t *testing.T) {
	e := newEnv(t)
	e.seed() // 没有保存过部署设置：容器名未设置
	nVersions, nAudits := len(e.repo.versions), len(e.repo.audits)
	check := func(name string, err error) {
		t.Helper()
		ee := mustErr(t, err, 409, "deploy_not_set")
		if ee.Message != notSetMsg {
			t.Fatalf("%s：提示不对：%s", name, ee.Message)
		}
		if len(e.repo.versions) != nVersions || len(e.repo.audits) != nAudits {
			t.Fatalf("%s：被拒绝时不能写版本或审计", name)
		}
		if e.repo.lock != nil || e.files.get(testPath) != baseConf || e.rs.count() != 0 {
			t.Fatalf("%s：被拒绝时不能留锁、改文件或执行命令：%v", name, e.rs.commands())
		}
		if e.svc.JobView() != nil {
			t.Fatalf("%s：不应产生重启流程", name)
		}
	}
	// 保存并重启
	_, err := e.svc.Save(bg, e.actor, SaveRequest{Content: e.edit("5000", "6000"), BaseVersion: 1, Restart: true})
	check("保存并重启", err)
	// 即使强制、即使确认了所有提示，也不能绕过
	_, err = e.svc.Save(bg, e.actor, SaveRequest{Content: e.edit("5000", "6000"), BaseVersion: 1, Restart: true, Force: true, AckDrift: true, AckWarnings: true})
	check("保存并重启（强制）", err)
	// 仅重启
	_, err = e.svc.Restart(bg, e.actor, RestartRequest{Force: true})
	check("仅重启", err)
	// 回滚
	_, err = e.svc.Rollback(bg, e.actor, 1, RollbackRequest{BaseVersion: 1, Force: true})
	check("回滚", err)

	// “仅保存（不重启）”照常可用：写版本、写文件，但不重启
	res, err := e.svc.Save(bg, e.actor, SaveRequest{Content: e.edit("5000", "6000"), BaseVersion: 1})
	if err != nil || res.Restarting || res.Version != 2 || res.Status != StatusDraft {
		t.Fatalf("%v %+v", err, res)
	}
	if !strings.Contains(e.files.get(testPath), "6000") || e.rs.count() != 0 {
		t.Fatal("仅保存应写文件且不执行命令")
	}
	// 之后仍然不能重启使它生效
	_, err = e.svc.Restart(bg, e.actor, RestartRequest{})
	mustErr(t, err, 409, "deploy_not_set")
	if e.rs.count() != 0 {
		t.Fatal("未设置容器名不能执行任何命令")
	}
	// 设置容器名后一切恢复
	e.useDocker()
	if _, err := e.svc.Restart(bg, e.actor, RestartRequest{}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if got := e.rs.commands(); len(got) != 1 || got[0] != "docker restart goinception" || e.repo.version(2).Status != StatusApplied {
		t.Fatalf("%v %+v", got, e.repo.version(2))
	}
}

func TestRestartActionsRejectedWithInvalidStoredContainer(t *testing.T) {
	e := newEnv(t)
	e.seed()
	// 库里被人改成了非法的容器名（或字段残缺）：同样不能执行命令
	e.repo.deploy = &Deploy{Container: "a b; reboot", RestartTimeoutSec: 60, HealthTimeoutSec: 60, AutoRollback: true}
	_, err := e.svc.Save(bg, e.actor, SaveRequest{Content: e.edit("5000", "6000"), BaseVersion: 1, Restart: true})
	mustErr(t, err, 409, "deploy_invalid")
	_, err = e.svc.Restart(bg, e.actor, RestartRequest{})
	mustErr(t, err, 409, "deploy_invalid")
	_, err = e.svc.Rollback(bg, e.actor, 1, RollbackRequest{BaseVersion: 1})
	mustErr(t, err, 409, "deploy_invalid")
	if e.rs.count() != 0 || len(e.repo.versions) != 1 {
		t.Fatalf("不能执行命令或写版本：%v", e.rs.commands())
	}
	o := e.overview()
	if o.Deploy.Configured || o.Deploy.PreviewError == "" {
		t.Fatalf("概览应指出容器名不合法：%+v", o.Deploy)
	}
	// 仅保存仍可用
	if _, err := e.svc.Save(bg, e.actor, SaveRequest{Content: e.edit("5000", "6000"), BaseVersion: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestRestartRequiresConnConfig(t *testing.T) {
	e := newEnv(t)
	e.useDocker()
	e.seed()
	delete(e.repo.cfg, cfgHost)
	_, err := e.svc.Save(bg, e.actor, SaveRequest{Content: e.edit("5000", "6000"), BaseVersion: 1, Restart: true})
	mustErr(t, err, 400, "bad_request")
	if len(e.repo.versions) != 1 {
		t.Fatal("没写版本")
	}
	_, err = e.svc.Restart(bg, e.actor, RestartRequest{})
	mustErr(t, err, 400, "bad_request")
}

// ---- 有任务在执行 ----

func busyTasks(n int) Tasks {
	return Tasks{Known: true, Connections: n, Running: n, Items: []Task{{ID: "1", Info: "alter table t"}}}
}

func TestBusyRequiresConfirmation(t *testing.T) {
	e := newEnv(t)
	e.useDocker()
	e.seed()
	e.pr.tasks = busyTasks(2)
	content := e.edit("5000", "6000")
	_, err := e.svc.Save(bg, e.actor, SaveRequest{Content: content, BaseVersion: 1, Restart: true})
	ee := mustErr(t, err, 409, "busy")
	if tk, ok := ee.Data.(Tasks); !ok || tk.Running != 2 {
		t.Fatalf("应带上任务信息：%+v", ee.Data)
	}
	if len(e.repo.versions) != 1 || e.files.get(testPath) != baseConf || e.rs.count() != 0 {
		t.Fatal("需要确认时不能写版本、改文件或重启")
	}
	// 强制重启
	res, err := e.svc.Save(bg, e.actor, SaveRequest{Content: content, BaseVersion: 1, Restart: true, Force: true})
	if err != nil || !res.Restarting {
		t.Fatalf("%v %+v", err, res)
	}
	e.svc.Wait()
	if e.repo.version(2).Status != StatusApplied {
		t.Fatal("强制重启应成功")
	}
	// 仅重启同样需要确认
	_, err = e.svc.Restart(bg, e.actor, RestartRequest{})
	mustErr(t, err, 409, "busy")
}

func TestWaitForTasksThenRestart(t *testing.T) {
	e := newEnv(t)
	e.useDocker()
	e.seed()
	e.pr.tasksFn = func(call int) Tasks {
		if call <= 3 {
			return busyTasks(1)
		}
		return Tasks{Known: true}
	}
	res, err := e.svc.Save(bg, e.actor, SaveRequest{Content: e.edit("5000", "6000"), BaseVersion: 1, Restart: true, WaitTasks: true})
	if err != nil || !res.Restarting {
		t.Fatalf("%v %+v", err, res)
	}
	e.svc.Wait()
	if v := e.repo.version(2); v.Status != StatusApplied {
		t.Fatalf("%+v", v)
	}
	if s := e.svc.JobView().Steps[0]; s.Name != "等待执行中的任务完成" || s.Status != "ok" {
		t.Fatalf("%+v", s)
	}
}

func TestWaitForTasksTimeout(t *testing.T) {
	e := newEnv(t)
	d := DefaultDeploy()
	d.Container, d.WaitIdleMaxSec = testContainer, 5
	_ = e.repo.SaveDeploy(d, "root")
	e.seed()
	e.pr.tasks = busyTasks(1)
	if _, err := e.svc.Save(bg, e.actor, SaveRequest{Content: e.edit("5000", "6000"), BaseVersion: 1, Restart: true, WaitTasks: true}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	v := e.repo.version(2)
	if v.Status != StatusFailed || !strings.Contains(v.Error, "未重启") || e.rs.count() != 0 || e.files.get(testPath) != baseConf {
		t.Fatalf("%+v 重启 %d 次", v, e.rs.count())
	}
}

func TestUnknownTasksDoNotBlock(t *testing.T) {
	e := newEnv(t)
	e.useDocker()
	e.seed()
	e.pr.tasks = Tasks{Known: false, Note: "不支持"}
	if _, err := e.svc.Save(bg, e.actor, SaveRequest{Content: e.edit("5000", "6000"), BaseVersion: 1, Restart: true}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if e.repo.version(2).Status != StatusApplied {
		t.Fatal("判断不了任务时不应阻塞")
	}
}

// ---- 并发保护 ----

func TestOnlyOneFlowAtATime(t *testing.T) {
	e := newEnv(t)
	e.useDocker()
	e.seed()
	e.rs.block, e.rs.started = make(chan struct{}), make(chan struct{}, 1)
	if _, err := e.svc.Save(bg, e.actor, SaveRequest{Content: e.edit("5000", "6000"), BaseVersion: 1, Restart: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-e.rs.started:
	case <-time.After(5 * time.Second):
		t.Fatal("重启没有开始")
	}
	if j := e.svc.JobView(); j == nil || j.Done {
		t.Fatalf("流程应正在进行：%+v", j)
	}
	_, err := e.svc.Save(bg, e.actor, SaveRequest{Content: "[inc]\n", BaseVersion: 2})
	mustErr(t, err, 409, "busy_op")
	_, err = e.svc.Restart(bg, e.actor, RestartRequest{})
	mustErr(t, err, 409, "busy_op")
	_, err = e.svc.Rollback(bg, e.actor, 1, RollbackRequest{BaseVersion: 2})
	mustErr(t, err, 409, "busy_op")
	_, err = e.svc.ImportDisk(e.actor, ImportRequest{BaseVersion: 2})
	mustErr(t, err, 409, "busy_op")
	close(e.rs.block)
	e.svc.Wait()
	if e.repo.lock != nil {
		t.Fatal("流程结束后锁必须释放")
	}
	if _, err := e.svc.Save(bg, e.actor, SaveRequest{Content: e.edit("6000", "7000"), BaseVersion: 2}); err != nil {
		t.Fatal(err)
	}
}

func TestOtherInstanceHoldsLock(t *testing.T) {
	e := newEnv(t)
	e.seed()
	if _, ok, _, _ := e.repo.AcquireLock("alice", time.Hour); !ok {
		t.Fatal()
	}
	_, err := e.svc.Save(bg, e.actor, SaveRequest{Content: e.edit("5000", "6000"), BaseVersion: 1})
	ee := mustErr(t, err, 409, "busy_op")
	if !strings.Contains(ee.Message, "alice") {
		t.Fatal(ee.Message)
	}
	if e.svc.opMu.TryLock() {
		e.svc.opMu.Unlock()
	} else {
		t.Fatal("拿不到库里的锁时必须释放进程内的锁")
	}
}

// ---- 回滚、仅重启、导入 ----

func TestRollbackCreatesNewVersion(t *testing.T) {
	e := newEnv(t)
	e.useDocker()
	e.seed()
	if _, err := e.svc.Save(bg, e.actor, SaveRequest{Content: e.edit("5000", "6000"), BaseVersion: 1, Restart: true}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	res, err := e.svc.Rollback(bg, e.actor, 1, RollbackRequest{BaseVersion: 2})
	if err != nil || !res.Restarting || res.Version != 3 {
		t.Fatalf("%v %+v", err, res)
	}
	e.svc.Wait()
	v := e.repo.version(3)
	if v.Status != StatusApplied || !strings.HasPrefix(v.ChangeSummary, "回滚到 #1：") || !strings.Contains(v.ChangeSummary, "max_update_rows: 6000 → 5000") || v.Content != baseConf {
		t.Fatalf("%+v", v)
	}
	if e.files.get(testPath) != baseConf || e.svc.JobView().Kind != "rollback" {
		t.Fatal("磁盘应回到 #1 的内容")
	}
	if a := e.repo.auditText(); !strings.Contains(a, actRollback) {
		t.Fatalf("回滚应写审计：%s", a)
	}
}

func TestRollbackGuards(t *testing.T) {
	e := newEnv(t)
	e.useDocker()
	e.seed()
	_, err := e.svc.Rollback(bg, e.actor, 9, RollbackRequest{BaseVersion: 1})
	mustErr(t, err, 404, "not_found")
	_, err = e.svc.Rollback(bg, e.actor, 1, RollbackRequest{BaseVersion: 0})
	mustErr(t, err, 409, "stale")
	_, err = e.svc.Rollback(bg, e.actor, 1, RollbackRequest{BaseVersion: 1})
	mustErr(t, err, 400, "bad_request") // 与当前生效内容相同
	// 失败过的版本不能作为回滚目标
	e.rs.failOn[1], e.rs.failOn[2] = errors.New("x"), errors.New("y")
	_, _ = e.svc.Save(bg, e.actor, SaveRequest{Content: e.edit("5000", "6000"), BaseVersion: 1, Restart: true})
	e.svc.Wait()
	if e.repo.version(2).Status != StatusFailed {
		t.Fatal(e.repo.version(2).Status)
	}
	_, err = e.svc.Rollback(bg, e.actor, 2, RollbackRequest{BaseVersion: 2})
	mustErr(t, err, 400, "bad_request")
	// 自动恢复过的版本同样不能作为回滚目标
	e.repo.versions[1].Status = StatusRolledBack
	_, err = e.svc.Rollback(bg, e.actor, 2, RollbackRequest{BaseVersion: 2})
	mustErr(t, err, 400, "bad_request")
	// 回滚前也要重新校验：被篡改成非法内容的版本不能回滚
	e.repo.versions[0].Content = "[inc\n"
	e.repo.versions = append(e.repo.versions, &Version{ID: 99, Version: 3, Content: baseConf, Status: StatusApplied})
	_, err = e.svc.Rollback(bg, e.actor, 1, RollbackRequest{BaseVersion: 3})
	mustErr(t, err, 400, "bad_request")
}

func TestRestartOnly(t *testing.T) {
	e := newEnv(t)
	e.useDocker()
	e.seed()
	res, err := e.svc.Restart(bg, e.actor, RestartRequest{})
	if err != nil || !res.Restarting || res.Version != 0 {
		t.Fatalf("%v %+v", err, res)
	}
	e.svc.Wait()
	if e.rs.count() != 1 || e.svc.JobView().Kind != "restart" || !e.svc.JobView().OK || len(e.repo.versions) != 1 {
		t.Fatalf("%+v", e.svc.JobView())
	}
	// 失败：没有版本可恢复，记录失败
	e.rs.err = errors.New("Error response from daemon: No such container: goinception")
	if _, err := e.svc.Restart(bg, e.actor, RestartRequest{}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if j := e.svc.JobView(); j.OK || j.Result != "failed" || !strings.Contains(j.Message, "No such container") || e.rs.count() != 2 {
		t.Fatalf("%+v", j)
	}
	if rec := e.svc.lastRestart(); rec == nil || rec.OK {
		t.Fatalf("%+v", rec)
	}
}

func TestRestartAppliesPendingDraft(t *testing.T) {
	e := newEnv(t)
	e.useDocker()
	e.seed()
	if _, err := e.svc.Save(bg, e.actor, SaveRequest{Content: e.edit("5000", "6000"), BaseVersion: 1}); err != nil {
		t.Fatal(err)
	}
	// 磁盘被人改过：先要确认
	e.files.set(testPath, "# tampered\n")
	_, err := e.svc.Restart(bg, e.actor, RestartRequest{})
	mustErr(t, err, 409, "drift")
	if _, err := e.svc.Restart(bg, e.actor, RestartRequest{AckDrift: true}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if v := e.repo.version(2); v.Status != StatusApplied || e.files.get(testPath) != v.Content {
		t.Fatalf("%+v", v)
	}
}

func TestImportDisk(t *testing.T) {
	e := newEnv(t)
	e.seed()
	_, err := e.svc.ImportDisk(e.actor, ImportRequest{BaseVersion: 1})
	mustErr(t, err, 400, "bad_request") // 已一致
	e.files.set(testPath, strings.Replace(baseConf, "5000", "7777", 1))
	_, err = e.svc.ImportDisk(e.actor, ImportRequest{BaseVersion: 0})
	mustErr(t, err, 409, "stale")
	res, err := e.svc.ImportDisk(e.actor, ImportRequest{BaseVersion: 1})
	if err != nil || res.Status != StatusDraft {
		t.Fatalf("%v %+v", err, res)
	}
	v := e.repo.version(2)
	if !strings.Contains(v.ChangeSummary, "手工修改") || !strings.Contains(v.ChangeSummary, "5000 → 7777") {
		t.Fatalf("%q", v.ChangeSummary)
	}
	if e.overview().File.Drift.Kind != "none" {
		t.Fatal("导入后应一致")
	}
	// 以“已生效”导入
	e.files.set(testPath, strings.Replace(baseConf, "5000", "8888", 1))
	res, err = e.svc.ImportDisk(e.actor, ImportRequest{BaseVersion: 2, AsApplied: true})
	if err != nil || res.Status != StatusApplied || e.overview().Effective.Version != 3 {
		t.Fatalf("%v %+v", err, res)
	}
	e.files.files = map[string]string{}
	_, err = e.svc.ImportDisk(e.actor, ImportRequest{BaseVersion: 3})
	mustErr(t, err, 404, "not_found")
	if !strings.Contains(e.repo.auditText(), actImport) {
		t.Fatal("应写审计")
	}
}

func TestImportFirstTime(t *testing.T) {
	e := newEnv(t)
	e.files.set(testPath, baseConf)
	res, err := e.svc.ImportDisk(e.actor, ImportRequest{AsApplied: true})
	if err != nil || res.Version != 1 || !strings.Contains(e.repo.version(1).ChangeSummary, "首次纳入") {
		t.Fatalf("%v %+v", err, res)
	}
}

// ---- 版本查看、对比、状态 ----

func TestVersionDetailAndDiff(t *testing.T) {
	e := newEnv(t)
	e.seed()
	if _, err := e.svc.Save(bg, e.actor, SaveRequest{Content: e.edit("check_dml_limit = false", "check_dml_limit = true"), BaseVersion: 1,
		Secrets: map[string]string{"inc.backup_password": "other"}}); err != nil {
		t.Fatal(err)
	}
	d, err := e.svc.GetVersion(2)
	if err != nil || strings.Contains(d.Content, "other") || strings.Contains(d.Content, "s3cret") || d.Label != "已保存，待重启生效" {
		t.Fatalf("%v %+v", err, d)
	}
	df, err := e.svc.DiffVersions(2, 0)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range df.Diff.Lines {
		if l.Op != " " {
			lines = append(lines, l.Op+l.Text)
		}
	}
	got := strings.Join(lines, "|")
	for _, w := range []string{"-check_dml_limit = false", "+check_dml_limit = true", "已修改"} {
		if !strings.Contains(got, w) {
			t.Errorf("对比缺少 %q：%s", w, got)
		}
	}
	if strings.Contains(got, "other") || strings.Contains(got, "s3cret") {
		t.Fatalf("对比泄露密码：%s", got)
	}
	if df.From == nil || df.From.Version != 1 || df.To.Version != 2 {
		t.Fatalf("%+v %+v", df.From, df.To)
	}
	// 第一个版本与空文件对比：全部新增
	df, _ = e.svc.DiffVersions(1, 0)
	if df.From != nil || df.Diff.Removed != 0 || df.Diff.Added == 0 {
		t.Fatalf("%+v", df.Diff)
	}
	_, err = e.svc.GetVersion(99)
	mustErr(t, err, 404, "not_found")
	_, err = e.svc.DiffVersions(99, 0)
	mustErr(t, err, 404, "not_found")
}

func TestPreviewDiff(t *testing.T) {
	e := newEnv(t)
	e.seed()
	res, err := e.svc.PreviewDiff(e.edit("5000", "6000"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Diff.Added != 1 || res.Diff.Removed != 1 || res.From == nil || res.From.Version != 1 {
		t.Fatalf("%+v", res)
	}
	// 密码占位符没有基线时也能出对比（不报错）
	e.files.files = map[string]string{}
	e.repo.versions = nil
	if _, err := e.svc.PreviewDiff(MaskContent(baseConf), nil); err != nil {
		t.Fatal(err)
	}
}

func TestStatus(t *testing.T) {
	e := newEnv(t)
	e.seed()
	e.pr.tasks = busyTasks(2)
	st, err := e.svc.Status(bg)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "running" || st.Version != "v1.2.7" || st.Running == nil || *st.Running != 2 || st.Effective != 1 || st.Drift.Kind != "none" {
		t.Fatalf("%+v", st)
	}
	e.pr.tasks = Tasks{Known: false, Note: "语句不支持"}
	st, _ = e.svc.Status(bg)
	if st.Running != nil || st.TaskNote != "语句不支持" {
		t.Fatalf("取不到任务数应为未知：%+v", st)
	}
	e.pr.setUp(false)
	st, _ = e.svc.Status(bg)
	if st.State != "down" || !strings.Contains(st.Message, "refused") {
		t.Fatalf("%+v", st)
	}
	delete(e.repo.cfg, cfgHost)
	st, _ = e.svc.Status(bg)
	if st.State != "unknown" || st.StateLabel != "未知" {
		t.Fatalf("%+v", st)
	}
}

// 页面源文件把密码显示成占位符：“校验”先还原再校验，不应对没改动的密码误报。
func TestValidateEditRestoresPlaceholder(t *testing.T) {
	e := newEnv(t)
	e.seed()
	masked := MaskContent(e.files.get(testPath))
	if !strings.Contains(masked, MaskPlaceholder) {
		t.Fatalf("测试前提：源文件里应有密码占位符：\n%s", masked)
	}
	if r := Validate(masked); r.OK() {
		t.Fatal("直接校验带占位符的原文应报错（保存前必须还原）")
	}
	if r := e.svc.ValidateEdit(masked); !r.OK() {
		t.Fatalf("校验前应还原占位符：%+v", r.Issues)
	}
	// 没有原值可还原（密码参数不在任何基线里）：占位符照常报错
	fresh := "[inc]\nbackup_host = \"10.0.0.1\"\nbackup_user = \"u\"\nbackup_other_password = \"" + MaskPlaceholder + "\"\n"
	if r := e.svc.ValidateEdit(fresh); r.OK() {
		t.Fatal("找不到原值的占位符仍应报错")
	}
}
