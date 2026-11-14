// @author: kevinkelly
// @license: Apache Licence
// @file: instances_test.go
// @time: 2026/10/28

package instmgr

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dbtool/internal/model"
)

// wantErr 断言 err 是业务错误，状态码一致，信息包含子串。
func wantErr(t *testing.T, err error, status int, contains string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("期望业务错误 %d %q，实际 %v", status, contains, err)
	}
	if e.Status != status || !strings.Contains(e.Message, contains) {
		t.Fatalf("期望 %d 含 %q，实际 %d %q (%s)", status, contains, e.Status, e.Message, e.Code)
	}
}

func goodInput() InstanceInput {
	return InstanceInput{Name: "prod-mysql-03", Role: "master", DBType: "mysql", Host: "10.20.0.13", Port: 3306, User: "dba_ro", Password: "p@ss-W0rd!",
		VerifySSL: true, GroupIDs: []uint{1}, TagIDs: []uint{1, 2}}
}

func TestListInstancesScopeAndFilters(t *testing.T) {
	e := newEnv()
	e.repo.instances[1].Tags = []model.InstanceTag{{ID: 2, TagCode: "can_write", Active: true}}
	e.repo.instances[4].IsEnable = true
	all, err := e.svc.ListInstances(root, ListQuery{})
	if err != nil || all.Total != 5 || len(all.Items) != 5 {
		t.Fatalf("超级用户应看到全部 5 个: %+v %v", all, err)
	}
	mine, _ := e.svc.ListInstances(bob, ListQuery{})
	if mine.Total != 3 {
		t.Fatalf("普通用户只能看到资源组内的 3 个，实际 %d", mine.Total)
	}
	for _, it := range mine.Items {
		if it.ID == 2 || it.ID == 5 {
			t.Fatalf("不应看到实例 %d", it.ID)
		}
	}
	// 没有任何资源组的用户看不到实例
	none, _ := e.svc.ListInstances(Caller{ID: 77, Username: "x"}, ListQuery{})
	if none.Total != 0 || len(none.Items) != 0 {
		t.Fatalf("无关联用户应为空: %+v", none)
	}
	if r, _ := e.svc.ListInstances(root, ListQuery{DBType: "doris"}); r.Total != 1 || r.Items[0].DBTypeLabel != "Doris" {
		t.Fatalf("按类型筛选: %+v", r)
	}
	if r, _ := e.svc.ListInstances(root, ListQuery{Tag: "can_write"}); r.Total != 1 || r.Items[0].ID != 1 {
		t.Fatalf("按标签筛选: %+v", r)
	}
	if r, _ := e.svc.ListInstances(root, ListQuery{Status: "1"}); r.Total != 1 || r.Items[0].ID != 4 {
		t.Fatalf("按采集状态筛选: %+v", r)
	}
	if r, _ := e.svc.ListInstances(root, ListQuery{Q: "mssql"}); r.Total != 1 {
		t.Fatalf("关键字: %+v", r)
	}
	if r, _ := e.svc.ListInstances(root, ListQuery{PageSize: 2, Page: 3}); len(r.Items) != 1 || r.Total != 5 {
		t.Fatalf("分页: %+v", r)
	}
	if r, _ := e.svc.ListInstances(root, ListQuery{PageSize: 100000}); len(r.Items) != 5 {
		t.Fatalf("超大分页应被限制但仍可用: %+v", r)
	}
	_, err = e.svc.ListInstances(root, ListQuery{Role: "boss"})
	wantErr(t, err, 400, "角色")
	_, err = e.svc.ListInstances(root, ListQuery{Status: "x"})
	wantErr(t, err, 400, "状态")
	// 列表项不暴露密码
	if strings.Contains(strings.Join([]string{all.Items[0].User, all.Items[0].Host}, ""), "secret-pw") {
		t.Fatal("列表不应含密码")
	}
}

func TestChoices(t *testing.T) {
	e := newEnv()
	c, err := e.svc.Choices(bob, "accounts")
	if err != nil || len(c) != 3 {
		t.Fatalf("bob 的实例下拉: %+v %v", c, err)
	}
	sup := map[uint]bool{}
	for _, it := range c {
		sup[it.ID] = it.Supported
	}
	if !sup[1] || sup[3] || sup[4] {
		t.Fatalf("账号管理只支持 mysql: %+v", sup)
	}
	c, _ = e.svc.Choices(root, "sessions")
	for _, it := range c {
		if it.ID == 4 && !it.Supported {
			t.Fatal("doris 支持会话管理")
		}
		if it.ID == 3 && it.Supported {
			t.Fatal("mssql 不支持会话管理")
		}
	}
	_, err = e.svc.Choices(root, "nope")
	wantErr(t, err, 400, "未知")
}

func TestSaveInstanceCreate(t *testing.T) {
	e := newEnv()
	it, err := e.svc.SaveInstance(root, 0, goodInput())
	if err != nil {
		t.Fatal(err)
	}
	if it.Name != "prod-mysql-03" || it.ID == 0 {
		t.Fatalf("返回: %+v", it)
	}
	w := e.repo.writes[0]
	if w.Password == nil || *w.Password != "p@ss-W0rd!" || len(w.GroupIDs) != 1 || len(w.TagIDs) != 2 {
		t.Fatalf("写入内容: %+v", w)
	}
	a := e.repo.lastAudit()
	if a.Action != "新增实例" || !strings.Contains(a.ExtraInfo, "prod-mysql-03") || !strings.Contains(a.ExtraInfo, "can_read,can_write") {
		t.Fatalf("审计: %+v", a)
	}
	if strings.Contains(a.ExtraInfo, "p@ss-W0rd!") {
		t.Fatal("审计里不能有密码")
	}
	if a.UserID != 1 || a.UserName != "root" {
		t.Fatalf("审计用户: %+v", a)
	}
}

// 并发创建同名实例时，InstanceNameTaken 之后才被唯一索引拦住：应当是 409 而不是 500（真库冒烟发现）。
func TestSaveInstanceDuplicateKeyIsConflict(t *testing.T) {
	e := newEnv()
	e.repo.saveErr = errors.New("Error 1062 (23000): Duplicate entry 'prod-mysql-03' for key 'dbt_sql_instance.instance_name'")
	_, err := e.svc.SaveInstance(root, 0, goodInput())
	wantErr(t, err, 409, "实例名称已存在")
	if len(e.repo.audits) != 0 {
		t.Fatal("没保存成功不应写审计")
	}
}

func TestSaveInstanceValidation(t *testing.T) {
	cases := []struct {
		name string
		mod  func(in *InstanceInput)
		want string
	}{
		{"空名称", func(in *InstanceInput) { in.Name = "  " }, "实例名称"},
		{"名称过长", func(in *InstanceInput) { in.Name = strings.Repeat("a", 51) }, "过长"},
		{"名称含换行", func(in *InstanceInput) { in.Name = "a\nb" }, "控制字符"},
		{"角色", func(in *InstanceInput) { in.Role = "x" }, "角色"},
		{"类型", func(in *InstanceInput) { in.DBType = "foo" }, "数据库类型"},
		{"模式", func(in *InstanceInput) { in.Mode = "weird" }, "模式"},
		{"HOST 含空格", func(in *InstanceInput) { in.Host = "a b" }, "HOST"},
		{"HOST 含注入", func(in *InstanceInput) { in.Host = "x';drop" }, "HOST"},
		{"端口 0", func(in *InstanceInput) { in.Port = 0 }, "PORT"},
		{"端口过大", func(in *InstanceInput) { in.Port = 70000 }, "PORT"},
		{"缺用户", func(in *InstanceInput) { in.User = "" }, "用户名"},
		{"缺密码", func(in *InstanceInput) { in.Password = "" }, "密码"},
		{"字符集非法", func(in *InstanceInput) { in.Charset = "utf8 mb4" }, "字符集"},
		{"正则非法", func(in *InstanceInput) { in.ShowRegex = "(" }, "正则"},
		{"隐藏正则非法", func(in *InstanceInput) { in.DeniedRegex = "[" }, "正则"},
		{"资源组不存在", func(in *InstanceInput) { in.GroupIDs = []uint{99} }, "资源组"},
		{"标签不存在", func(in *InstanceInput) { in.TagIDs = []uint{99} }, "标签"},
		{"隧道不存在", func(in *InstanceInput) { in.TunnelID = 9 }, "隧道"},
	}
	for _, c := range cases {
		e := newEnv()
		in := goodInput()
		c.mod(&in)
		_, err := e.svc.SaveInstance(root, 0, in)
		if err == nil {
			t.Errorf("%s：应该失败", c.name)
			continue
		}
		wantErr(t, err, 400, c.want)
		if len(e.repo.writes) != 0 {
			t.Errorf("%s：校验失败不能写库", c.name)
		}
	}
	// 免认证类型可以不填用户和密码
	e := newEnv()
	in := goodInput()
	in.Name, in.DBType, in.User, in.Password = "redis-1", "redis", "", ""
	if _, err := e.svc.SaveInstance(root, 0, in); err != nil {
		t.Fatalf("redis 不填账号密码应通过: %v", err)
	}
	// 名称重复
	in = goodInput()
	in.Name = "prod-mysql-01"
	_, err := e.svc.SaveInstance(root, 0, in)
	wantErr(t, err, 409, "已存在")
}

func TestSaveInstanceUpdateKeepsPassword(t *testing.T) {
	e := newEnv()
	in := goodInput()
	in.Name, in.Password = "prod-mysql-01", ""
	if _, err := e.svc.SaveInstance(root, 1, in); err != nil {
		t.Fatal(err)
	}
	if e.repo.writes[0].Password != nil {
		t.Fatal("编辑时密码留空表示不修改，应传 nil")
	}
	if e.repo.instances[1].Password != "secret-pw" {
		t.Fatal("密码被误改")
	}
	if a := e.repo.lastAudit(); a.Action != "修改实例" || strings.Contains(a.ExtraInfo, "已设置新密码") {
		t.Fatalf("审计: %+v", a)
	}
	in.Password = "n3w-P@ssword"
	if _, err := e.svc.SaveInstance(root, 1, in); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.repo.lastAudit().ExtraInfo, "已设置新密码") || strings.Contains(e.repo.lastAudit().ExtraInfo, "n3w-P@ssword") {
		t.Fatalf("改密审计应只说明改过: %s", e.repo.lastAudit().ExtraInfo)
	}
	_, err := e.svc.SaveInstance(root, 999, in)
	wantErr(t, err, 404, "不存在")
}

func TestEnableToggle(t *testing.T) {
	e := newEnv()
	if err := e.svc.SetInstanceEnable(root, 1, true); err != nil {
		t.Fatal(err)
	}
	if !e.repo.instances[1].IsEnable || e.repo.lastAudit().Action != "切换慢日志采集" {
		t.Fatalf("%+v %+v", e.repo.instances[1], e.repo.lastAudit())
	}
	wantErr(t, e.svc.SetInstanceEnable(root, 99, true), 404, "不存在")
}

func TestDeleteInstance(t *testing.T) {
	e := newEnv()
	wantErr(t, e.svc.DeleteInstance(root, 1, false), 400, "确认")
	e.repo.refs[1] = []Ref{{Label: "SQL 工单", Count: 3, Blocking: true}, {Label: "查询权限", Count: 2, Blocking: true, Hint: "先回收"}, {Label: "账号登记", Count: 4}}
	err := e.svc.DeleteInstance(root, 1, true)
	wantErr(t, err, 409, "SQL 工单 3 条")
	if !strings.Contains(err.Error(), "查询权限 2 条（先回收）") || strings.Contains(err.Error(), "账号登记") {
		t.Fatalf("提示应列出阻塞引用、不含配置类引用: %v", err)
	}
	if len(e.repo.deleted) != 0 {
		t.Fatal("被引用时不能删除")
	}
	e.repo.refs[1] = []Ref{{Label: "账号登记", Count: 4}}
	if err := e.svc.DeleteInstance(root, 1, true); err != nil {
		t.Fatal(err)
	}
	if len(e.repo.deleted) != 1 || e.repo.lastAudit().Action != "删除实例" {
		t.Fatalf("%v %+v", e.repo.deleted, e.repo.lastAudit())
	}
	// 普通用户不可见的实例按“未关联”处理
	wantErr(t, e.svc.DeleteInstance(bob, 2, true), 403, "未关联")
}

func TestInstanceRefsList(t *testing.T) {
	e := newEnv()
	refs, err := e.svc.InstanceRefs(root, 1)
	if err != nil || refs == nil || len(refs) != 0 {
		t.Fatalf("没有引用应返回空数组: %#v %v", refs, err)
	}
}

func TestTestInstanceMySQL(t *testing.T) {
	e := newEnv().withVersion("8.0.36")
	r, err := e.svc.TestInstance(context.Background(), root, 1)
	if err != nil {
		t.Fatal(err)
	}
	if r.Version != "8.0.36" || r.Endpoint != "10.0.0.1:3306" || len(e.dialer.dialed) != 1 {
		t.Fatalf("%+v", r)
	}
	if e.dialer.dialed[0].Password != "secret-pw" || !e.target.closed {
		t.Fatalf("应用保存的密码连接并关闭连接: %+v closed=%v", e.dialer.dialed[0], e.target.closed)
	}
	e.dialer.dialErr = errors.New("dial tcp 10.0.0.1:3306: i/o timeout")
	_, err = e.svc.TestInstance(context.Background(), root, 1)
	wantErr(t, err, 502, "无法连接实例")
}

func TestTestInstanceOtherEngine(t *testing.T) {
	e := newEnv()
	r, err := e.svc.TestInstance(context.Background(), root, 3)
	if err != nil || r.Version != "SQL Server 2019" || len(e.dialer.probed) != 1 || len(e.dialer.dialed) != 0 {
		t.Fatalf("mssql 应走 Probe: %+v %v", r, err)
	}
	e.dialer.probeErr = errors.New("暂不支持的数据库类型: pgsql")
	_, err = e.svc.TestInstance(context.Background(), root, 3)
	wantErr(t, err, 400, "暂不支持")
}

func TestTestFormUsesSavedPasswordWhenBlank(t *testing.T) {
	e := newEnv().withVersion("5.7.44")
	in := goodInput()
	in.Password = ""
	if _, err := e.svc.TestForm(context.Background(), root, 1, in); err != nil {
		t.Fatal(err)
	}
	if got := e.dialer.dialed[0]; got.Password != "secret-pw" || got.Host != "10.20.0.13" {
		t.Fatalf("留空应沿用已保存的密码，其余用表单值: %+v", got)
	}
	// 新建时必须填密码
	_, err := e.svc.TestForm(context.Background(), root, 0, in)
	wantErr(t, err, 400, "密码")
	// 表单里的新密码优先
	in.Password = "typed-pw"
	if _, err := e.svc.TestForm(context.Background(), root, 1, in); err != nil || e.dialer.dialed[1].Password != "typed-pw" {
		t.Fatalf("%v %+v", err, e.dialer.dialed)
	}
	// 隧道
	e.repo.tunnel = &TunnelInfo{Host: "jump", Port: 22, User: "u"}
	in.TunnelID = 1
	if _, err := e.svc.TestForm(context.Background(), root, 1, in); err != nil || e.dialer.dialed[2].Tunnel == nil || e.dialer.dialed[2].Tunnel.Host != "jump" {
		t.Fatalf("隧道应带上: %v %+v", err, e.dialer.dialed[2])
	}
	in.TunnelID = 9
	_, err = e.svc.TestForm(context.Background(), root, 1, in)
	wantErr(t, err, 400, "隧道")
	in.TunnelID = 0
	in.Host = ""
	_, err = e.svc.TestForm(context.Background(), root, 1, in)
	wantErr(t, err, 400, "HOST")
}

// ---- Binlog ----

func binlogEnv() *env {
	e := newEnv()
	e.target.on("SHOW BINARY LOGS", rs([]string{"log_name", "file_size", "encrypted"},
		[]any{"mysql-bin.000812", "1073741824", "No"}, []any{"mysql-bin.000813", "1073741824", "No"}, []any{"mysql-bin.000814", "641728512", "No"}))
	e.target.on("Binlog Dump", rs([]string{"n"}, []any{int64(2)}))
	return e
}

func TestBinlogListAndPreview(t *testing.T) {
	e := binlogEnv()
	files, err := e.svc.ListBinlogs(context.Background(), root, 1)
	if err != nil || len(files) != 3 || files[0].Size != 1073741824 {
		t.Fatalf("%+v %v", files, err)
	}
	p, err := e.svc.PreviewPurge(context.Background(), root, 1, "mysql-bin.000814")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Purge) != 2 || p.PurgeSize != 2*1073741824 || p.Keep != 1 || p.DumpThreads != 2 || p.SQL != "PURGE BINARY LOGS TO 'mysql-bin.000814'" {
		t.Fatalf("%+v", p)
	}
	if len(e.target.execs()) != 0 {
		t.Fatal("预览不能执行任何语句")
	}
	_, err = e.svc.PreviewPurge(context.Background(), root, 1, "mysql-bin.000812")
	wantErr(t, err, 400, "最早")
	_, err = e.svc.PreviewPurge(context.Background(), root, 1, "mysql-bin.000999")
	wantErr(t, err, 404, "不存在")
	_, err = e.svc.PreviewPurge(context.Background(), root, 1, "x'; DROP")
	wantErr(t, err, 400, "不合法")
}

func TestBinlogPurge(t *testing.T) {
	e := binlogEnv()
	_, err := e.svc.PurgeBinlogs(context.Background(), root, 1, "mysql-bin.000813", "")
	wantErr(t, err, 400, "不一致")
	_, err = e.svc.PurgeBinlogs(context.Background(), root, 1, "mysql-bin.000813", "mysql-bin.000814")
	wantErr(t, err, 400, "不一致")
	if len(e.target.execs()) != 0 {
		t.Fatal("确认不一致时不能执行")
	}
	p, err := e.svc.PurgeBinlogs(context.Background(), root, 1, "mysql-bin.000813", "mysql-bin.000813")
	if err != nil {
		t.Fatal(err)
	}
	ex := e.target.execs()
	if len(ex) != 1 || ex[0].sql != "PURGE BINARY LOGS TO ?" || ex[0].args[0] != "mysql-bin.000813" || len(p.Purge) != 1 {
		t.Fatalf("%+v", ex)
	}
	a := e.repo.lastAudit()
	if a.Action != "清理Binlog" || !strings.Contains(a.ExtraInfo, "mysql-bin.000813") || !strings.Contains(a.ExtraInfo, "成功") {
		t.Fatalf("%+v", a)
	}
	// 失败也要审计
	e.target.execErr["PURGE"] = errors.New("Error 1373: Target log not found")
	_, err = e.svc.PurgeBinlogs(context.Background(), root, 1, "mysql-bin.000813", "mysql-bin.000813")
	wantErr(t, err, 502, "1373")
	if !strings.Contains(e.repo.lastAudit().ExtraInfo, "失败") {
		t.Fatalf("失败也应记审计: %+v", e.repo.lastAudit())
	}
}

func TestBinlogOnlyMySQL(t *testing.T) {
	e := binlogEnv()
	_, err := e.svc.ListBinlogs(context.Background(), root, 5) // polardb
	wantErr(t, err, 400, "暂不支持")
	_, err = e.svc.PreviewPurge(context.Background(), root, 3, "x")
	wantErr(t, err, 400, "暂不支持")
	_, err = e.svc.PurgeBinlogs(context.Background(), root, 4, "a", "a")
	wantErr(t, err, 400, "暂不支持")
	if len(e.dialer.dialed) != 0 {
		t.Fatal("不支持的类型不能去连目标库")
	}
}
