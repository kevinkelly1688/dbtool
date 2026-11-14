// @author: kevinkelly
// @license: Apache Licence
// @file: audit_gap_test.go
// @time: 2026/10/28

package instmgr

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dbtool/internal/audit"
	"dbtool/internal/model"
)

// S5a-A1 / S5a-A2：测试连接与被拒绝的高危操作都要写审计。

func auditsOf(e *env, action string) []model.AuditEntry {
	var out []model.AuditEntry
	for _, a := range e.repo.audits {
		if a.Action == action {
			out = append(out, a)
		}
	}
	return out
}

func TestTestConnectionAudited(t *testing.T) {
	e := newEnv().withVersion("8.0.36")
	if _, err := e.svc.TestInstance(context.Background(), root, 1); err != nil {
		t.Fatal(err)
	}
	as := auditsOf(e, "测试连接")
	if len(as) != 1 || as[0].UserName != root.Username || !strings.Contains(as[0].ExtraInfo, "10.0.0.1:3306") || !strings.Contains(as[0].ExtraInfo, "结果：成功") {
		t.Fatalf("成功应写审计: %+v", e.repo.audits)
	}
	if strings.Contains(as[0].ExtraInfo, "secret-pw") {
		t.Fatal("审计不能含密码")
	}
	e.dialer.dialErr = errors.New("dial tcp 10.0.0.1:3306: i/o timeout")
	if _, err := e.svc.TestInstance(context.Background(), root, 1); err == nil {
		t.Fatal("应失败")
	}
	as = auditsOf(e, "测试连接")
	if len(as) != 2 || !strings.Contains(as[1].ExtraInfo, "结果：失败") || !strings.Contains(as[1].ExtraInfo, "i/o timeout") {
		t.Fatalf("失败也应写审计: %+v", as)
	}
	if got := audit.Classify(as[1].Action, as[1].ExtraInfo); got.Result != audit.ResultFailed || got.Code != "instance.test" || got.Category != audit.CatInst {
		t.Errorf("失败测试的归类: %+v", got)
	}
	if got := audit.Classify(as[0].Action, as[0].ExtraInfo); got.Result != audit.ResultOK {
		t.Errorf("成功测试的归类: %+v", got)
	}
}

func TestTestFormAuditedWithoutPassword(t *testing.T) {
	e := newEnv().withVersion("5.7.44")
	in := goodInput()
	in.Password = "typed-pw-XYZ"
	if _, err := e.svc.TestForm(context.Background(), root, 0, in); err != nil {
		t.Fatal(err)
	}
	as := auditsOf(e, "测试连接")
	if len(as) != 1 || !strings.Contains(as[0].ExtraInfo, in.Host) || !strings.Contains(as[0].ExtraInfo, "结果：成功") {
		t.Fatalf("%+v", e.repo.audits)
	}
	// 驱动报错里带了密码也要抹掉
	e.dialer.dialErr = errors.New("Access denied for user using password typed-pw-XYZ")
	if _, err := e.svc.TestForm(context.Background(), root, 0, in); err == nil {
		t.Fatal("应失败")
	}
	for _, a := range auditsOf(e, "测试连接") {
		if strings.Contains(a.ExtraInfo, "typed-pw-XYZ") {
			t.Fatalf("审计不能含密码: %q", a.ExtraInfo)
		}
	}
	if as = auditsOf(e, "测试连接"); len(as) != 2 || !strings.Contains(as[1].ExtraInfo, "结果：失败") {
		t.Fatalf("%+v", as)
	}
	// 表单校验没通过（没有真正去连）不记
	n := len(e.repo.audits)
	bad := in
	bad.Host = ""
	if _, err := e.svc.TestForm(context.Background(), root, 0, bad); err == nil {
		t.Fatal("应失败")
	}
	if len(e.repo.audits) != n {
		t.Errorf("没有发起连接不应写审计: %+v", e.repo.audits[n:])
	}
}

func TestDeleteReferencedInstanceRefusalAudited(t *testing.T) {
	e := newEnv()
	e.repo.refs[1] = []Ref{{Label: "SQL 工单", Count: 3, Blocking: true}}
	if err := e.svc.DeleteInstance(root, 1, true); err == nil {
		t.Fatal("被引用应拒绝")
	}
	as := auditsOf(e, "删除实例（拒绝）")
	if len(as) != 1 || !strings.Contains(as[0].ExtraInfo, "SQL 工单 3 条") || !strings.Contains(as[0].ExtraInfo, "原因") {
		t.Fatalf("%+v", e.repo.audits)
	}
	if len(auditsOf(e, "删除实例")) != 0 {
		t.Error("被拒绝不能记成删除成功")
	}
	got := audit.Classify(as[0].Action, as[0].ExtraInfo)
	if got.Result != audit.ResultDenied || got.Code != "instance.delete" || !got.Risky {
		t.Errorf("归类: %+v", got)
	}
	// 没有确认（前端没勾）属于参数错误，不记
	n := len(e.repo.audits)
	_ = e.svc.DeleteInstance(root, 1, false)
	if len(e.repo.audits) != n {
		t.Error("未确认不应写审计")
	}
}

func TestKillRefusedAudited(t *testing.T) {
	e := procEnv()
	// 只选了平台自己的连接和复制线程：全部被拒，不执行；写“终止会话（拒绝）”，不写成功的“终止会话”
	res, err := e.svc.Kill(context.Background(), root, 1, []KillTarget{{ID: 10}, {ID: 12}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.target.execs()) != 0 || res[0].OK || res[1].OK {
		t.Fatalf("不应执行: %+v %+v", e.target.execs(), res)
	}
	as := auditsOf(e, "终止会话（拒绝）")
	if len(as) != 1 || !strings.Contains(as[0].ExtraInfo, "线程 10") || !strings.Contains(as[0].ExtraInfo, "线程 12") || !strings.Contains(as[0].ExtraInfo, "原因") {
		t.Fatalf("%+v", e.repo.audits)
	}
	if len(auditsOf(e, "终止会话")) != 0 {
		t.Errorf("全部被拒绝时不应写“终止会话”: %+v", e.repo.audits)
	}
	if got := audit.Classify(as[0].Action, as[0].ExtraInfo); got.Result != audit.ResultDenied || got.Code != "session.kill" {
		t.Errorf("归类: %+v", got)
	}
	// 一部分可终止、一部分被拒：两条都写
	e = procEnv()
	if _, err := e.svc.Kill(context.Background(), root, 1, []KillTarget{{ID: 13}, {ID: 10}}, true); err != nil {
		t.Fatal(err)
	}
	if len(auditsOf(e, "终止会话")) != 1 || len(auditsOf(e, "终止会话（拒绝）")) != 1 {
		t.Fatalf("%+v", e.repo.audits)
	}
	// 已经结束的会话不是“被拒绝”
	e = procEnv()
	if _, err := e.svc.Kill(context.Background(), root, 1, []KillTarget{{ID: 99}}, true); err != nil {
		t.Fatal(err)
	}
	if len(auditsOf(e, "终止会话（拒绝）")) != 0 {
		t.Errorf("已结束不应记为拒绝: %+v", e.repo.audits)
	}
}

func TestParamBlacklistRefusalAudited(t *testing.T) {
	for _, c := range []struct{ param, value, want string }{
		{"init_connect", "SET @a=1", "不允许在线修改"},
		{"read_only", "ON", "没有开启"},
	} {
		e := paramEnv()
		if _, err := e.svc.SetParam(context.Background(), root, 1, c.param, c.value, true); err == nil {
			t.Fatalf("%s 应拒绝", c.param)
		}
		as := auditsOf(e, "修改实例参数（拒绝）")
		if len(as) != 1 || !strings.Contains(as[0].ExtraInfo, c.param) || !strings.Contains(as[0].ExtraInfo, c.want) {
			t.Fatalf("%s: %+v", c.param, e.repo.audits)
		}
		if got := audit.Classify(as[0].Action, as[0].ExtraInfo); got.Result != audit.ResultDenied || got.Code != "param.edit" {
			t.Errorf("归类: %+v", got)
		}
	}
	// 普通的校验失败（超范围）不是“被拒绝的高危操作”，不记
	e := paramEnv()
	if _, err := e.svc.SetParam(context.Background(), root, 1, "max_connections", "100001", true); err == nil {
		t.Fatal("应失败")
	}
	if len(e.repo.audits) != 0 {
		t.Errorf("校验失败不应写审计: %+v", e.repo.audits)
	}
}

func TestProtectedAccountRefusalAudited(t *testing.T) {
	e := accEnv()
	ctx := context.Background()
	pw := PasswordReq{User: "mysql.sys", Host: "localhost", Password: "N3w-Passw0rd!", Password2: "N3w-Passw0rd!", Confirm: true}
	cases := []struct {
		name, action string
		run          func() error
	}{
		{"改密", "修改数据库账号密码（拒绝）", func() error { return e.svc.ResetPassword(ctx, root, 1, pw) }},
		{"锁定", "锁定数据库账号（拒绝）", func() error {
			return e.svc.SetAccountLock(ctx, root, 1, LockReq{User: "mysql.sys", Host: "localhost", Lock: true, Confirm: true})
		}},
		{"解锁", "解锁数据库账号（拒绝）", func() error {
			return e.svc.SetAccountLock(ctx, root, 1, LockReq{User: "mysql.sys", Host: "localhost", Lock: false, Confirm: true})
		}},
		{"删除", "删除数据库账号（拒绝）", func() error {
			return e.svc.DropAccount(ctx, root, 1, DropReq{User: "dba_ro", Host: "10.0.0.2", Confirm: true})
		}},
		{"授权", "账号授权（拒绝）", func() error {
			_, err := e.svc.Grant(ctx, root, 1, GrantReq{User: "mysql.sys", Host: "localhost", Op: "grant", Scope: "global", Privs: []string{"SELECT"}, Confirm: true})
			return err
		}},
		{"回收", "账号回收权限（拒绝）", func() error {
			_, err := e.svc.Grant(ctx, root, 1, GrantReq{User: "dba_ro", Host: "10.0.0.2", Op: "revoke", Scope: "global", Privs: []string{"SELECT"}, Confirm: true})
			return err
		}},
	}
	for _, c := range cases {
		if err := c.run(); err == nil {
			t.Fatalf("%s：应被拒绝", c.name)
		}
		as := auditsOf(e, c.action)
		if len(as) != 1 || !strings.Contains(as[0].ExtraInfo, "原因") || !strings.Contains(as[0].ExtraInfo, "不能对账号") {
			t.Fatalf("%s：%+v", c.name, e.repo.audits)
		}
		if got := audit.Classify(as[0].Action, as[0].ExtraInfo); got.Result != audit.ResultDenied || got.Code == "" {
			t.Errorf("%s 归类: %+v", c.name, got)
		}
		if strings.Contains(as[0].ExtraInfo, "N3w-Passw0rd") {
			t.Errorf("%s：审计不能含密码", c.name)
		}
	}
	// 账号不存在（404）不是“被拒绝的高危操作”
	n := len(e.repo.audits)
	if err := e.svc.DropAccount(ctx, root, 1, DropReq{User: "ghost", Host: "x", Confirm: true}); err == nil {
		t.Fatal("应失败")
	}
	if len(e.repo.audits) != n {
		t.Errorf("账号不存在不应写拒绝审计: %+v", e.repo.audits[n:])
	}
}

func TestCreateAccountReservedNameRefusalAudited(t *testing.T) {
	e := accEnv()
	in := AccountCreate{User: "root", Host: "localhost", Password: "Passw0rd!x", Password2: "Passw0rd!x", Confirm: true}
	_, err := e.svc.CreateAccounts(context.Background(), root, 1, in)
	wantErr(t, err, 403, "内置账号")
	if len(e.target.execs()) != 0 || len(e.dialer.dialed) != 0 {
		t.Fatal("命中保留名不能去库里执行")
	}
	as := auditsOf(e, "创建数据库账号（拒绝）")
	if len(as) != 1 || !strings.Contains(as[0].ExtraInfo, "root") || !strings.Contains(as[0].ExtraInfo, "原因") || strings.Contains(as[0].ExtraInfo, "Passw0rd") {
		t.Fatalf("%+v", e.repo.audits)
	}
	if got := audit.Classify(as[0].Action, as[0].ExtraInfo); got.Result != audit.ResultDenied || got.Code != "account.create" {
		t.Errorf("归类: %+v", got)
	}
	// 同名但不是内置主机（root@10.%）照常创建
	in.Host = "10.1.%"
	if _, err := e.svc.CreateAccounts(context.Background(), root, 1, in); err != nil {
		t.Fatalf("root@10.1.%% 不是内置账号: %v", err)
	}
}
