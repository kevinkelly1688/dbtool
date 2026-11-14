// @author: kevinkelly
// @license: Apache Licence
// @file: service_test.go
// @time: 2026/10/28

package binlog

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/replication"

	"dbtool/internal/audit"
	"dbtool/internal/settings"
)

func apiErr(t *testing.T, err error, code string) {
	t.Helper()
	var be *Error
	if !errors.As(err, &be) {
		t.Fatalf("期望业务错误 %s，实际 %v", code, err)
	}
	if be.Code != code {
		t.Fatalf("错误码 %s（%s），期望 %s", be.Code, be.Message, code)
	}
}

func req(mode string) Req {
	return Req{InstanceID: 1, Mode: mode, StartFile: "bin.000001"}
}

func TestInstancesVisibility(t *testing.T) {
	svc, _, repo := newSvc(t, newFakeSource(scenario(), t))
	list, err := svc.Instances(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].Name != "mysql-a" || list[1].Name != "mysql-b" || list[2].Name != "mysql-tunnel" {
		t.Fatalf("超级用户应看到全部 MySQL 实例（不含 mssql）: %+v", list)
	}
	if list[2].Unsupported != "" {
		t.Errorf("隧道实例经 EngineConn 自动走隧道，不应再标注不支持: %+v", list[2])
	}
	list, _ = svc.Instances(alice)
	if len(list) != 1 || list[0].ID != 1 {
		t.Fatalf("普通用户只看资源组内的: %+v", list)
	}
	repo.perms[3] = map[string]bool{permQueryAllInsts: true}
	list, _ = svc.Instances(carol)
	if len(list) != 3 {
		t.Fatalf("query_all_instances 看全部: %+v", list)
	}
	list, _ = svc.Instances(audit0(99))
	if len(list) != 0 {
		t.Fatalf("没有任何资源组: %+v", list)
	}
}

func TestInstanceAccessChecks(t *testing.T) {
	svc, _, _ := newSvc(t, newFakeSource(scenario(), t))
	ctx := context.Background()
	if _, err := svc.Info(ctx, alice, 2); err == nil {
		t.Fatal("不在资源组内的实例不能读")
	} else {
		apiErr(t, err, "no_instance")
	}
	_, err := svc.Info(ctx, root, 3)
	apiErr(t, err, "unsupported")
	// 隧道实例（ID 4）：连接信息由 repo.EngineConn 改写，Binlog 解析照常可用
	if _, err = svc.Info(ctx, root, 4); err != nil {
		t.Fatalf("配置了 SSH 隧道的实例应可解析: %v", err)
	}
	src := svc.src.(*fakeSource)
	if n := len(src.conns); n == 0 || src.conns[n-1].Host != "127.0.0.1" || src.conns[n-1].Port != 45678 {
		t.Fatalf("应使用 EngineConn 改写后的本地转发地址连接: %+v", src.conns)
	}
	_, err = svc.Info(ctx, root, 99)
	apiErr(t, err, "not_found")
	_, err = svc.Info(ctx, root, 0)
	apiErr(t, err, "bad_request")
}

func TestInfoRollbackFlags(t *testing.T) {
	src := newFakeSource(scenario(), t)
	svc, _, _ := newSvc(t, src)
	in, err := svc.Info(context.Background(), root, 1)
	if err != nil || !in.CanRollback || in.RollbackReason != "" || len(in.Files) != 2 {
		t.Fatalf("ROW+FULL 可回滚: %+v %v", in, err)
	}
	cases := []struct {
		mod  func(*ServerInfo)
		want string
	}{
		{func(i *ServerInfo) { i.RowImage = "MINIMAL" }, "binlog_row_image=MINIMAL"},
		{func(i *ServerInfo) { i.RowImage = "NOBLOB" }, "binlog_row_image=NOBLOB"},
		{func(i *ServerInfo) { i.Format = "STATEMENT" }, "binlog_format=STATEMENT"},
		{func(i *ServerInfo) { i.Format = "MIXED" }, "binlog_format=MIXED"},
		{func(i *ServerInfo) { i.LogBin = false }, "没有开启 binlog"},
		{func(i *ServerInfo) { i.Format = "" }, "读取不到 binlog_format"},
		{func(i *ServerInfo) { i.RowImage = "" }, "读取不到 binlog_row_image"},
	}
	for _, c := range cases {
		src.info = newFakeSource(scenario(), t).info
		c.mod(&src.info)
		in, err := svc.Info(context.Background(), root, 1)
		if err != nil || in.CanRollback || !strings.Contains(in.RollbackReason, c.want) {
			t.Errorf("%s: %+v %v", c.want, in, err)
		}
	}
}

func TestInfoErrors(t *testing.T) {
	src := newFakeSource(scenario(), t)
	svc, _, _ := newSvc(t, src)
	src.connectErr = errors.New("dial tcp: connect: Access denied for user with s3cretPW")
	_, err := svc.Info(context.Background(), root, 1)
	apiErr(t, err, "connect_failed")
	if strings.Contains(err.Error(), "s3cretPW") {
		t.Errorf("错误信息不能带密码: %v", err)
	}
	src.connectErr = nil
	src.infoErr = errors.New("Access denied; you need (at least one of) the SUPER, REPLICATION CLIENT privilege(s) for this operation")
	_, err = svc.Info(context.Background(), root, 1)
	apiErr(t, err, "db_error")
	if !strings.Contains(err.Error(), "REPLICATION CLIENT") {
		t.Errorf("应提示权限: %v", err)
	}
	if src.openSessions() != 0 {
		t.Errorf("会话要关闭: %d", src.openSessions())
	}
}

func TestDatabasesRespectVisibility(t *testing.T) {
	src := newFakeSource(scenario(), t)
	svc, _, repo := newSvc(t, src)
	repo.instances[0].DeniedDBNameRegex = "hidden"
	dbs, err := svc.Databases(context.Background(), root, 1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(dbs, ",") != "report,shop" {
		t.Errorf("系统库与隐藏库要去掉: %v", dbs)
	}
	repo.instances[0].DeniedDBNameRegex = ""
	repo.instances[0].ShowDBNameRegex = "shop"
	dbs, _ = svc.Databases(context.Background(), root, 1)
	if strings.Join(dbs, ",") != "shop" {
		t.Errorf("只显示 shop: %v", dbs)
	}
	repo.instances[0].ShowDBNameRegex = "("
	_, err = svc.Databases(context.Background(), root, 1)
	apiErr(t, err, "bad_config")
}

func TestStartParseAndRows(t *testing.T) {
	src := newFakeSource(scenario(), t)
	svc, au, _ := newSvc(t, src)
	r := req("")
	r.Databases = []string{"shop"}
	v, err := svc.Start(context.Background(), root, r)
	if err != nil {
		t.Fatal(err)
	}
	if v.Mode != ModeSQL || v.Status == "" || v.ID == "" || v.Instance != "mysql-a" || !strings.Contains(v.Range, "bin.000001:4 → 当前末尾") {
		t.Fatalf("起始视图: %+v", v)
	}
	if v.Limits.MaxRows != DefaultMaxRows || v.Limits.MaxBytes != DefaultMaxMB<<20 || v.Limits.TimeoutSec != DefaultTimeoutSec {
		t.Errorf("默认上限: %+v", v.Limits)
	}
	done := waitDone(t, svc, root, v.ID)
	if done.Status != StatusDone || done.Matched != 3 || done.Trx != 2 || done.ByType[KindUpdate] != 1 || done.Truncate != "" || !done.CanDownload || done.ExpiresAt == "" {
		t.Fatalf("完成视图: %+v", done)
	}
	if done.RemainSec <= 0 || done.RemainSec > int(jobTTL/time.Second) {
		t.Errorf("剩余保留秒数: %d", done.RemainSec)
	}
	page, err := svc.Rows(root, v.ID, 0, 0)
	if err != nil || page.Total != 3 || len(page.Rows) != 3 || page.Rows[0].Type != KindInsert {
		t.Fatalf("行: %+v %v", page, err)
	}
	page, _ = svc.Rows(root, v.ID, 2, 10)
	if len(page.Rows) != 1 || page.Rows[0].Type != KindDelete || page.Offset != 2 {
		t.Errorf("分页: %+v", page)
	}
	page, _ = svc.Rows(root, v.ID, 99, 10)
	if len(page.Rows) != 0 || page.Total != 3 {
		t.Errorf("越界页: %+v", page)
	}
	if acts := au.actions(); len(acts) != 1 || acts[0] != "Binlog解析-解析" {
		t.Errorf("审计: %v", acts)
	}
	e := au.last()
	if e.UserName != "root" || !strings.Contains(e.ExtraInfo, "mysql-a") || !strings.Contains(e.ExtraInfo, v.ID) {
		t.Errorf("审计内容: %+v", e)
	}
	// 连接都已关闭
	time.Sleep(20 * time.Millisecond)
	if src.openSessions() != 0 {
		t.Errorf("会话泄漏: %d", src.openSessions())
	}
}

func TestStartRollbackAndAudit(t *testing.T) {
	src := newFakeSource(scenario(), t)
	svc, au, _ := newSvc(t, src)
	v, err := svc.Start(context.Background(), root, req(ModeRollback))
	if err != nil {
		t.Fatal(err)
	}
	done := waitDone(t, svc, root, v.ID)
	if done.Status != StatusDone || done.Mode != ModeRollback {
		t.Fatalf("%+v", done)
	}
	page, _ := svc.Rows(root, v.ID, 0, 0)
	if !strings.HasPrefix(page.Rows[0].SQL, "INSERT INTO `shop`.`orders`") || !strings.HasPrefix(page.Rows[2].SQL, "DELETE FROM") {
		t.Errorf("回滚顺序: %+v", page.Rows)
	}
	if acts := au.actions(); len(acts) != 1 || acts[0] != "Binlog解析-回滚" {
		t.Errorf("回滚的审计动作名: %v", acts)
	}
}

func TestStartRefusesRollbackWhenUnsupported(t *testing.T) {
	src := newFakeSource(scenario(), t)
	src.info.RowImage = "MINIMAL"
	svc, au, _ := newSvc(t, src)
	_, err := svc.Start(context.Background(), root, req(ModeRollback))
	apiErr(t, err, "rollback_unsupported")
	if !strings.Contains(err.Error(), "MINIMAL") {
		t.Errorf("应说明原因: %v", err)
	}
	if len(au.actions()) != 0 {
		t.Errorf("被拒绝的请求不应写审计: %v", au.actions())
	}
	// 同样的实例仍可解析
	v, err := svc.Start(context.Background(), root, req(ModeSQL))
	if err != nil {
		t.Fatal(err)
	}
	if waitDone(t, svc, root, v.ID).Status != StatusDone {
		t.Error("解析应成功")
	}
	// binlog_format != ROW 同理
	src.info.RowImage, src.info.Format = "FULL", "MIXED"
	_, err = svc.Start(context.Background(), root, req(ModeRollback))
	apiErr(t, err, "rollback_unsupported")
	// 没开 binlog：连解析都不行
	src.info.LogBin = false
	_, err = svc.Start(context.Background(), root, req(ModeSQL))
	apiErr(t, err, "no_binlog")
	if src.openSessions() != 0 {
		t.Errorf("会话泄漏: %d", src.openSessions())
	}
}

func TestStartValidation(t *testing.T) {
	src := newFakeSource(scenario(), t)
	svc, au, _ := newSvc(t, src)
	cases := []struct {
		name string
		mod  func(*Req)
		code string
	}{
		{"未选实例", func(r *Req) { r.InstanceID = 0 }, "bad_request"},
		{"非法模式", func(r *Req) { r.Mode = "drop" }, "bad_request"},
		{"没有起始文件", func(r *Req) { r.StartFile = "" }, "bad_request"},
		{"文件名含路径", func(r *Req) { r.StartFile = "../etc/passwd" }, "bad_request"},
		{"起始文件不在列表", func(r *Req) { r.StartFile = "bin.000099" }, "bad_request"},
		{"结束文件不在列表", func(r *Req) { r.EndFile = "bin.000099" }, "bad_request"},
		{"结束文件早于起始", func(r *Req) { r.StartFile, r.EndFile = "bin.000002", "bin.000001" }, "bad_request"},
		{"起始位置太小", func(r *Req) { r.StartPos = 2 }, "bad_request"},
		{"起始位置超出文件", func(r *Req) { r.StartPos = 99999 }, "bad_request"},
		{"结束位置没有结束文件", func(r *Req) { r.EndPos = 100 }, "bad_request"},
		{"同文件结束位置不大于起始", func(r *Req) { r.StartPos, r.EndFile, r.EndPos = 100, "bin.000001", 50 }, "bad_request"},
		{"时间格式", func(r *Req) { r.StartTime = "昨天" }, "bad_request"},
		{"结束时间早于开始", func(r *Req) { r.StartTime, r.StopTime = "2023-01-02 00:00:00", "2023-01-01 00:00:00" }, "bad_request"},
		{"语句类型", func(r *Req) { r.SQLTypes = []string{"select"} }, "bad_request"},
		{"库名含控制字符", func(r *Req) { r.Databases = []string{"a\x01b"} }, "bad_request"},
		{"库名过长", func(r *Req) { r.Databases = []string{strings.Repeat("a", 300)} }, "bad_request"},
		{"过滤条件太多", func(r *Req) {
			for i := 0; i < 60; i++ {
				r.Tables = append(r.Tables, "t"+strings.Repeat("x", i%5)+string(rune('a'+i%26))+string(rune('a'+i/26)))
			}
		}, "bad_request"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := req("")
			c.mod(&r)
			_, err := svc.Start(context.Background(), root, r)
			apiErr(t, err, c.code)
		})
	}
	if len(au.actions()) != 0 {
		t.Errorf("校验失败不写审计: %v", au.actions())
	}
	if src.openSessions() != 0 {
		t.Errorf("会话泄漏: %d", src.openSessions())
	}
}

func TestStartNormalizesLimitsAndLists(t *testing.T) {
	src := newFakeSource(scenario(), t)
	svc, _, _ := newSvc(t, src)
	r := req("")
	r.MaxRows, r.MaxMB, r.TimeoutSec = 99999999, 99999, 99999
	r.Databases = []string{"Shop, report", "SHOP"}
	r.Tables = []string{"shop.orders\norders2"}
	r.SQLTypes = []string{"Insert", " delete ", "ddl", "INSERT"}
	r.StartTime = "2023-11-14T22:00"
	v, err := svc.Start(context.Background(), root, r)
	if err != nil {
		t.Fatal(err)
	}
	if v.Limits.MaxRows != DefaultHardRows || v.Limits.MaxBytes != DefaultHardMB<<20 || v.Limits.TimeoutSec != MaxTimeoutSec {
		t.Errorf("上限应夹到硬上限: %+v", v.Limits)
	}
	if !strings.Contains(v.Range, "库 Shop,report") || !strings.Contains(v.Range, "表 shop.orders,orders2") || !strings.Contains(v.Range, "类型 insert,delete,ddl") {
		t.Errorf("规范化后的范围: %s", v.Range)
	}
	waitDone(t, svc, root, v.ID)

	r = req("")
	r.MaxRows, r.MaxMB, r.TimeoutSec = -5, -1, 3
	v, err = svc.Start(context.Background(), root, r)
	if err != nil {
		t.Fatal(err)
	}
	if v.Limits.MaxRows != 1 || v.Limits.MaxBytes != 1<<20 || v.Limits.TimeoutSec != 10 {
		t.Errorf("下限: %+v", v.Limits)
	}
	waitDone(t, svc, root, v.ID)
}

func TestTimeFiltersUseServerTimezone(t *testing.T) {
	src := newFakeSource(scenario(), t)
	src.info.TZOffset = 8 * 3600 // 服务器是东八区：t0 = 22:13:20 UTC = 次日 06:13:20
	svc, _, _ := newSvc(t, src)
	r := req("")
	r.StartTime = "2023-11-15 06:13:25" // 在插入 (t0+1) 与更新 (t0+2) 之后，删除 (t0+10) 之前
	v, err := svc.Start(context.Background(), root, r)
	if err != nil {
		t.Fatal(err)
	}
	done := waitDone(t, svc, root, v.ID)
	page, _ := svc.Rows(root, v.ID, 0, 0)
	if done.Matched != 1 || page.Rows[0].Type != KindDelete || page.Rows[0].Time != "2023-11-15 06:13:30" {
		t.Errorf("时间应按服务器时区: %+v %+v", done, page.Rows)
	}
}

func TestVisibleDBsApplyToResults(t *testing.T) {
	src := newFakeSource(scenario(), t)
	svc, _, repo := newSvc(t, src)
	repo.instances[0].DeniedDBNameRegex = "shop"
	v, err := svc.Start(context.Background(), root, req(""))
	if err != nil {
		t.Fatal(err)
	}
	if done := waitDone(t, svc, root, v.ID); done.Matched != 0 {
		t.Errorf("被隐藏的库里的数据不能解析出来: %+v", done)
	}
}

// 回滚到条数上限：不给“只回滚一部分”的结果，也不失败，而是在事务边界截断成一段，并给出续接位置；
// 从续接位置继续得到下一段，各段合起来正好是完整的回滚。
func TestRollbackLimitMakesSegments(t *testing.T) {
	src := newFakeSource(scenario(), t)
	svc, _, _ := newSvc(t, src)
	r := req(ModeRollback)
	r.MaxRows = 2 // 事务 1 有 2 条语句刚好放下，事务 2 放不下
	v, err := svc.Start(context.Background(), root, r)
	if err != nil {
		t.Fatal(err)
	}
	done := waitDone(t, svc, root, v.ID)
	sg := done.Segment
	if done.Status != StatusDone || sg == nil || !sg.More || sg.Index != 1 || sg.Reason != "rows" || done.Matched != 2 || done.Trx != 1 {
		t.Fatalf("回滚到上限应在事务边界截断成一段: %+v", done)
	}
	if sg.NextFile != "bin.000001" || sg.NextPos <= 4 || !strings.Contains(done.Truncate, "本段在事务边界结束") ||
		!strings.Contains(done.Truncate, fmt.Sprintf("bin.000001:%d", sg.NextPos)) {
		t.Errorf("续接位置: %+v / %s", sg, done.Truncate)
	}
	page, _ := svc.Rows(root, v.ID, 0, 0)
	if page.Total != 2 || !done.CanDownload || !strings.HasPrefix(page.Rows[0].SQL, "UPDATE `shop`.`orders` SET `name` = 'apple'") ||
		!strings.HasPrefix(page.Rows[1].SQL, "DELETE FROM `shop`.`orders` WHERE `id` = 1") {
		t.Errorf("本段只含事务 1 的回滚（后发生的先撤销）: %+v", page)
	}
	_, body, err := svc.Download(root, v.ID)
	if err != nil || !strings.Contains(string(body), fmt.Sprintf("分段回滚：这是第 1 段，后面还有（续接位置 bin.000001:%d）", sg.NextPos)) ||
		!strings.Contains(string(body), "先执行最后一段，最后执行第 1 段") {
		t.Errorf("文件头要说明分段和执行顺序: %v\n%s", err, body)
	}

	// 从续接位置继续：第 2 段只含事务 2（删除的回滚），也是最后一段
	r2 := req(ModeRollback)
	r2.StartFile, r2.StartPos, r2.Segment = sg.NextFile, sg.NextPos, 2
	v2, err := svc.Start(context.Background(), root, r2)
	if err != nil {
		t.Fatal(err)
	}
	done2 := waitDone(t, svc, root, v2.ID)
	if done2.Status != StatusDone || done2.Matched != 1 || done2.Truncate != "" || done2.Segment == nil || done2.Segment.More || done2.Segment.Index != 2 {
		t.Fatalf("第 2 段: %+v", done2)
	}
	if !strings.Contains(done2.Range, "第 2 段") {
		t.Errorf("范围里要写明第几段: %s", done2.Range)
	}
	page2, _ := svc.Rows(root, v2.ID, 0, 0)
	if len(page2.Rows) != 1 || !strings.HasPrefix(page2.Rows[0].SQL, "INSERT INTO `shop`.`orders`") {
		t.Errorf("第 2 段的内容: %+v", page2)
	}
	_, body2, _ := svc.Download(root, v2.ID)
	if !strings.Contains(string(body2), "这是第 2 段，也是最后一段") {
		t.Errorf("最后一段的文件头: %s", body2)
	}

	// 连一个完整的事务都放不下：没法分段，失败并说明原因
	r3 := req(ModeRollback)
	r3.MaxRows = 1
	v3, _ := svc.Start(context.Background(), root, r3)
	done3 := waitDone(t, svc, root, v3.ID)
	if done3.Status != StatusFailed || done3.ErrorCode != "limit_exceeded" || !strings.Contains(done3.Error, "无法在事务边界分段") || done3.CanDownload {
		t.Errorf("第一个事务就超限应失败: %+v", done3)
	}

	// 同样的限制在解析模式只是截断
	r.Mode = ModeSQL
	v, _ = svc.Start(context.Background(), root, r)
	done = waitDone(t, svc, root, v.ID)
	if done.Status != StatusDone || done.Matched != 2 || done.Segment != nil || !strings.Contains(done.Truncate, "2 条的上限") {
		t.Errorf("解析模式截断: %+v", done)
	}
}

func TestStreamErrors(t *testing.T) {
	src := newFakeSource(scenario(), t)
	svc, _, _ := newSvc(t, src)
	src.streamErr = newErr(403, "no_privilege", "实例账号没有权限拉取 binlog，需要 REPLICATION SLAVE")
	v, err := svc.Start(context.Background(), root, req(""))
	if err != nil {
		t.Fatal(err)
	}
	done := waitDone(t, svc, root, v.ID)
	if done.Status != StatusFailed || done.ErrorCode != "no_privilege" || !strings.Contains(done.Error, "REPLICATION SLAVE") {
		t.Errorf("%+v", done)
	}
	// 非业务错误：不泄露内部细节以外的内容，仍然是失败
	src.streamErr = errors.New("boom with s3cretPW")
	v, _ = svc.Start(context.Background(), root, req(""))
	done = waitDone(t, svc, root, v.ID)
	if done.Status != StatusFailed || strings.Contains(done.Error, "s3cretPW") {
		t.Errorf("%+v", done)
	}
}

func TestCancel(t *testing.T) {
	src := newFakeSource(scenario(), t)
	src.block = make(chan struct{})
	svc, au, _ := newSvc(t, src)
	v, err := svc.Start(context.Background(), root, req(""))
	if err != nil {
		t.Fatal(err)
	}
	waitRunning(t, src, 1)
	if got, _ := svc.Job(root, v.ID); got.Status != StatusRunning {
		t.Fatalf("应在运行: %+v", got)
	}
	// 非所有者不能取消
	apiErr(t, svc.Cancel(alice, v.ID), "not_found")
	if err := svc.Cancel(root, v.ID); err != nil {
		t.Fatal(err)
	}
	done := waitDone(t, svc, root, v.ID)
	if done.Status != StatusCanceled {
		t.Fatalf("%+v", done)
	}
	apiErr(t, svc.Cancel(root, v.ID), "conflict")
	page, _ := svc.Rows(root, v.ID, 0, 0)
	if page.Total != 0 {
		t.Errorf("取消的任务不保存结果: %+v", page)
	}
	if acts := au.actions(); len(acts) != 2 || acts[1] != "Binlog解析-取消" {
		t.Errorf("取消要写审计: %v", acts)
	}
	time.Sleep(30 * time.Millisecond)
	if src.openSessions() != 0 {
		t.Errorf("会话泄漏: %d", src.openSessions())
	}
}

func TestTimeout(t *testing.T) {
	old := timeoutUnit
	timeoutUnit = 10 * time.Millisecond // TimeoutSec 的下限是 10 → 100ms
	defer func() { timeoutUnit = old }()
	src := newFakeSource(scenario(), t)
	src.block = make(chan struct{})
	svc, _, _ := newSvc(t, src)
	// 解析模式：超时前已解析到的部分保留，并标记截断
	r := req("")
	r.TimeoutSec = 10
	v, err := svc.Start(context.Background(), root, r)
	if err != nil {
		t.Fatal(err)
	}
	done := waitDone(t, svc, root, v.ID)
	if done.Status != StatusDone || done.Matched != 3 || !strings.Contains(done.Truncate, "超过 10 秒被终止") {
		t.Errorf("解析超时应保留已解析部分: %+v", done)
	}
	// 回滚模式：不给不完整的结果，但超时不再直接失败：在事务边界结束成一段，给出续接位置
	r.Mode = ModeRollback
	v, err = svc.Start(context.Background(), root, r)
	if err != nil {
		t.Fatal(err)
	}
	done = waitDone(t, svc, root, v.ID)
	if done.Status != StatusDone || done.Matched != 3 || done.Segment == nil || !done.Segment.More || done.Segment.Reason != "timeout" ||
		!strings.Contains(done.Truncate, "任务运行超过 10 秒") {
		t.Errorf("回滚超时应在事务边界结束成一段: %+v", done)
	}
}

func TestConcurrencyLimits(t *testing.T) {
	src := newFakeSource(scenario(), t)
	src.block = make(chan struct{})
	svc, _, _ := newSvc(t, src)
	defer close(src.block)
	for i := 0; i < maxActivePerUser; i++ {
		if _, err := svc.Start(context.Background(), alice, req("")); err != nil {
			t.Fatal(err)
		}
	}
	_, err := svc.Start(context.Background(), alice, req(""))
	apiErr(t, err, "too_many_jobs")
	if src.openSessions() != maxActivePerUser {
		t.Errorf("被拒绝的请求要关掉自己的会话: %d", src.openSessions())
	}
	// 另一个用户不受影响，直到全局上限
	svc.repo.(*fakeRepo).perms[3] = map[string]bool{permQueryAllInsts: true}
	for i := 0; i < maxActiveAll-maxActivePerUser; i++ {
		u := audit0(uint(10 + i/maxActivePerUser))
		svc.repo.(*fakeRepo).perms[u.ID] = map[string]bool{permQueryAllInsts: true}
		if _, err := svc.Start(context.Background(), u, req("")); err != nil {
			t.Fatalf("第 %d 个: %v", i, err)
		}
	}
	u := audit0(50)
	svc.repo.(*fakeRepo).perms[u.ID] = map[string]bool{permQueryAllInsts: true}
	_, err = svc.Start(context.Background(), u, req(""))
	apiErr(t, err, "busy")
}

func TestJobVisibilityAndExpiry(t *testing.T) {
	src := newFakeSource(scenario(), t)
	svc, _, repo := newSvc(t, src)
	repo.perms[3] = map[string]bool{permQueryAllInsts: true}
	v, _ := svc.Start(context.Background(), alice, req(""))
	waitDone(t, svc, alice, v.ID)
	if _, err := svc.Job(carol, v.ID); err == nil {
		t.Error("别人看不到")
	} else {
		apiErr(t, err, "not_found")
	}
	if _, err := svc.Rows(carol, v.ID, 0, 0); err == nil {
		t.Error("别人读不到行")
	}
	if got, err := svc.Job(root, v.ID); err != nil || got.Mine || got.Owner != "alice" {
		t.Errorf("超级用户可以看: %+v %v", got, err)
	}
	if n := len(svc.Jobs(carol)); n != 0 {
		t.Errorf("列表只含自己的: %d", n)
	}
	if n := len(svc.Jobs(alice)); n != 1 {
		t.Errorf("自己的任务: %d", n)
	}
	if n := len(svc.Jobs(root)); n != 1 {
		t.Errorf("超级用户看全部: %d", n)
	}
	// 过期清理
	svc.now = func() time.Time { return time.Now().Add(jobTTL + time.Minute) }
	svc.gc()
	if _, err := svc.Job(alice, v.ID); err == nil {
		t.Error("过期任务应已清理")
	}
}

func TestJobStoreEvictsByBytes(t *testing.T) {
	st := newJobStore()
	now := time.Now()
	st.jobs["old"] = doneJob("old", 60<<20, 0, now.Add(-10*time.Minute))
	st.jobs["new"] = doneJob("new", 30<<20, 0, now.Add(-5*time.Minute))
	// 新任务按自己的体积上限（8 MB）预留内存：60 + 30 + 8 超过 96 MB 的内存预算，要淘汰最早结束的
	if err := st.add(&Job{ID: "x", UserID: 1, Status: StatusQueued, Limits: Limits{MaxBytes: 8 << 20}}, now, storeBudget(8)); err != nil {
		t.Fatal(err)
	}
	if st.get("old") != nil || st.get("new") == nil || st.get("x") == nil {
		t.Errorf("应淘汰最早结束的任务: old=%v new=%v", st.get("old") != nil, st.get("new") != nil)
	}
	// 全部在运行时无法淘汰
	st2 := newJobStore()
	for i := 0; i < maxStoredJobs; i++ {
		st2.jobs[string(rune('a'+i))] = &Job{ID: string(rune('a' + i)), UserID: uint(100 + i), Status: StatusRunning}
	}
	if err := st2.add(&Job{ID: "z", UserID: 1}, now, storeBudget(8)); err == nil {
		t.Error("没有可淘汰的任务时应拒绝")
	}
}

// 体积硬上限调大到 GB 级后，预算随之放大；运行中的任务按各自的体积上限预留，不会让总内存超出预算。
func TestJobStoreBudgetScalesAndReservesRunningJobs(t *testing.T) {
	if got := storeBudget(8); got != minStoreBytes {
		t.Errorf("硬上限很小时预算不低于 %d：%d", minStoreBytes, got)
	}
	if got := storeBudget(1024); got != 2<<30 {
		t.Errorf("1024 MB 的硬上限，磁盘预算是两份满额结果（2 GB）：%d", got)
	}
	budget := storeBudget(1024)
	now := time.Now()
	st := newJobStore()
	running := func(id string, user uint, mb int) *Job {
		return &Job{ID: id, UserID: user, Status: StatusRunning, Limits: Limits{MaxBytes: mb << 20}}
	}
	// 大任务的结果会转存磁盘：内存只预留 8 MB，磁盘按体积上限预留
	if err := st.add(running("a", 1, 1024), now, budget); err != nil {
		t.Fatalf("第一个 1 GB 的任务应放得下: %v", err)
	}
	if err := st.add(running("b", 2, 1024), now, budget); err != nil {
		t.Fatalf("两个 1 GB 的任务刚好是磁盘预算（2 GB）: %v", err)
	}
	err := st.add(running("c", 3, 1024), now, budget)
	var e *Error
	if !errors.As(err, &e) || e.Code != "busy" {
		t.Fatalf("磁盘预算被运行中的任务占满，没有可淘汰的，应拒绝为 busy：%v", err)
	}
	// 小任务的结果留在内存里，不占磁盘预算，不受影响
	if err := st.add(running("s", 3, 1), now, budget); err != nil {
		t.Fatalf("1 MB 的小任务只占内存预算，应放得下: %v", err)
	}
	// 其中一个跑完、结果在磁盘上只有 100 MB：释放出来的预留可以给新任务
	st.jobs["a"].Status, st.jobs["a"].Finished, st.jobs["a"].res = StatusDone, now, doneJob("a", 0, 100<<20, now).res
	if err := st.add(running("d", 4, 512), now, budget); err != nil {
		t.Fatalf("结束的任务按实际体积计，应放得下: %v", err)
	}
}

// doneJob 造一个已结束的任务，结果占 mem 字节内存、disk 字节磁盘（只改计数，不真的存数据）。
func doneJob(id string, mem int, disk int64, fin time.Time) *Job {
	st := newStmtStore("", 0)
	st.memBytes, st.diskBytes = mem, disk
	return &Job{ID: id, UserID: 1, Status: StatusDone, Finished: fin, res: &scanResult{Stmts: st}}
}

// 系统设置里的上限生效：请求里的值夹到上限；默认值不会超过上限；设置不合法或没设时用默认值；超出登记范围的夹回范围内。
func TestHardLimitsFromSettings(t *testing.T) {
	svc, _, repo := newSvc(t, newFakeSource(scenario(), t))
	if r, mb := svc.HardLimits(); r != DefaultHardRows || mb != DefaultHardMB {
		t.Errorf("没设置时用默认值: %d %d", r, mb)
	}
	repo.cfg = map[string]string{settings.KeyBinlogMaxRows: "1000", settings.KeyBinlogMaxResultMB: "4"}
	if r, mb := svc.HardLimits(); r != 1000 || mb != 4 {
		t.Errorf("取系统设置: %d %d", r, mb)
	}
	for _, bad := range []string{"", "abc", "0", "-5", "1.5"} {
		repo.cfg = map[string]string{settings.KeyBinlogMaxRows: bad, settings.KeyBinlogMaxResultMB: bad}
		if r, mb := svc.HardLimits(); r != DefaultHardRows || mb != DefaultHardMB {
			t.Errorf("%q 不是正整数，应回落到默认值: %d %d", bad, r, mb)
		}
	}
	repo.cfg = map[string]string{settings.KeyBinlogMaxRows: "999999999", settings.KeyBinlogMaxResultMB: "99999999"}
	if r, mb := svc.HardLimits(); r != 10_000_000 || mb != 8192 {
		t.Errorf("超出登记范围应夹到登记的最大值: %d %d", r, mb)
	}

	repo.cfg = map[string]string{settings.KeyBinlogMaxRows: "1000", settings.KeyBinlogMaxResultMB: "4"}
	r := req("")
	r.MaxRows, r.MaxMB = 999999, 999
	v, err := svc.Start(context.Background(), root, r)
	if err != nil {
		t.Fatal(err)
	}
	if v.Limits.MaxRows != 1000 || v.Limits.MaxBytes != 4<<20 {
		t.Errorf("请求值应夹到系统设置的上限: %+v", v.Limits)
	}
	waitDone(t, svc, root, v.ID)

	// 上限比页面默认值（500 条 / 2 MB）还小：默认值也不能超过上限
	repo.cfg = map[string]string{settings.KeyBinlogMaxRows: "100", settings.KeyBinlogMaxResultMB: "1"}
	v, err = svc.Start(context.Background(), root, req(""))
	if err != nil {
		t.Fatal(err)
	}
	if v.Limits.MaxRows != 100 || v.Limits.MaxBytes != 1<<20 {
		t.Errorf("没填时用 min(默认值, 上限): %+v", v.Limits)
	}
	waitDone(t, svc, root, v.ID)
}

// 代码里的兜底默认值必须和登记的默认值一致，否则读不到设置时页面显示的上限与系统配置页对不上。
func TestDefaultHardLimitsMatchRegistry(t *testing.T) {
	for key, want := range map[string]int{settings.KeyBinlogMaxRows: DefaultHardRows, settings.KeyBinlogMaxResultMB: DefaultHardMB} {
		d := settings.Find(key)
		if d == nil || d.Default != strconv.Itoa(want) {
			t.Errorf("%s 登记的默认值 %+v 与代码里的 %d 不一致", key, d, want)
		}
	}
}

func TestDownload(t *testing.T) {
	src := newFakeSource(scenario(), t)
	svc, au, _ := newSvc(t, src)
	v, _ := svc.Start(context.Background(), alice, req(ModeRollback))
	waitDone(t, svc, alice, v.ID)
	name, body, err := svc.Download(alice, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	txt := string(body)
	if !strings.HasPrefix(name, "binlog_1_rollback_") || !strings.HasSuffix(name, ".sql") {
		t.Errorf("文件名: %s", name)
	}
	for _, want := range []string{"-- Binlog 解析结果（回滚 SQL）", "实例: mysql-a", "操作人: alice", "语句数: 3    事务数: 2", "后发生的先撤销", "业务敏感数据"} {
		if !strings.Contains(txt, want) {
			t.Errorf("文件头缺少 %q:\n%s", want, txt)
		}
	}
	// 事务包装：回滚后顺序是 [事务2: INSERT]、[事务1: UPDATE, DELETE]
	body2 := txt[strings.Index(txt, "BEGIN;"):]
	want := "BEGIN;\nINSERT INTO `shop`.`orders` (`id`,`name`,`qty`,`note`) VALUES (2,'bob''s \\\\ cat',5,'n');\nCOMMIT;\nBEGIN;\nUPDATE `shop`.`orders` SET `name` = 'apple', `note` = NULL WHERE `id` = 1;\nDELETE FROM `shop`.`orders` WHERE `id` = 1;\nCOMMIT;\n"
	if body2 != want {
		t.Errorf("语句部分:\n%s\n期望:\n%s", body2, want)
	}
	if acts := au.actions(); len(acts) != 2 || acts[1] != "Binlog解析-下载" {
		t.Errorf("下载要写审计: %v", acts)
	}
	if e := au.last(); !strings.Contains(e.ExtraInfo, v.ID) || !strings.Contains(e.ExtraInfo, "语句数:3") {
		t.Errorf("下载审计内容: %+v", e)
	}
	// 每次下载都写一条
	_, _, _ = svc.Download(alice, v.ID)
	if len(au.actions()) != 3 {
		t.Errorf("第二次下载也要写审计: %v", au.actions())
	}
	// 别人下载不了；超级用户可以
	if _, _, err := svc.Download(carol, v.ID); err == nil {
		t.Error("别人不能下载")
	}
	if _, _, err := svc.Download(root, v.ID); err != nil {
		t.Errorf("超级用户可以下载: %v", err)
	}
}

func TestDownloadNotReady(t *testing.T) {
	src := newFakeSource(scenario(), t)
	src.block = make(chan struct{})
	svc, _, _ := newSvc(t, src)
	defer close(src.block)
	v, _ := svc.Start(context.Background(), root, req(""))
	waitRunning(t, src, 1)
	_, _, err := svc.Download(root, v.ID)
	apiErr(t, err, "not_ready")
	_, _, err = svc.Download(root, "nope")
	apiErr(t, err, "not_found")
}

func TestDownloadTruncatedNote(t *testing.T) {
	src := newFakeSource(scenario(), t)
	svc, _, _ := newSvc(t, src)
	r := req("")
	r.MaxRows = 1
	v, _ := svc.Start(context.Background(), root, r)
	waitDone(t, svc, root, v.ID)
	_, body, err := svc.Download(root, v.ID)
	if err != nil || !strings.Contains(string(body), "-- 注意: 结果达到 1 条的上限") {
		t.Errorf("截断要在文件里注明: %v\n%s", err, body)
	}
}

func TestAuditFailureDoesNotBreakParse(t *testing.T) {
	src := newFakeSource(scenario(), t)
	svc, au, _ := newSvc(t, src)
	au.err = errors.New("audit table gone")
	v, err := svc.Start(context.Background(), root, req(""))
	if err != nil {
		t.Fatalf("审计失败不应影响业务: %v", err)
	}
	if waitDone(t, svc, root, v.ID).Status != StatusDone {
		t.Error("任务应成功")
	}
}

func TestPanicInStreamIsContained(t *testing.T) {
	src := newFakeSource(scenario(), t)
	src.meta = &fakeMeta{cols: map[string][]colMeta{"shop.orders": nil}}
	svc, _, _ := newSvc(t, src)
	// 让 scan 内部 panic：事件流里塞一个没有 Header 的事件
	src.events = append(src.events, &replicationBad)
	v, err := svc.Start(context.Background(), root, req(""))
	if err != nil {
		t.Fatal(err)
	}
	done := waitDone(t, svc, root, v.ID)
	if done.Status != StatusFailed || done.ErrorCode != "internal" {
		t.Errorf("panic 应被捕获成失败: %+v", done)
	}
}

func audit0(id uint) audit.Actor { return audit.Actor{ID: id, Username: "u"} }

var replicationBad = replication.BinlogEvent{}

func TestEndFileWithoutPosUsesFileSize(t *testing.T) {
	src := newFakeSource(scenario(), t)
	svc, _, _ := newSvc(t, src)
	ctx := context.Background()
	inst, _ := svc.repo.GetInstance(1)
	info, _ := svc.Info(ctx, root, 1)
	r := req("")
	r.EndFile = "bin.000001"
	p, err := buildPlan(r, info, inst, DefaultHardRows, DefaultHardMB)
	if err != nil {
		t.Fatal(err)
	}
	if p.spec.EndPos != uint32(info.Files[0].Size) {
		t.Errorf("结束文件末尾应转成该文件的大小: %d vs %d", p.spec.EndPos, info.Files[0].Size)
	}
	r.EndPos = 77
	r.StartPos = 5
	p, _ = buildPlan(r, info, inst, DefaultHardRows, DefaultHardMB)
	if p.spec.EndPos != 77 {
		t.Errorf("显式结束位置优先: %d", p.spec.EndPos)
	}
}

func TestDisabledInstanceRefused(t *testing.T) {
	src := newFakeSource(scenario(), t)
	svc, _, _ := newSvc(t, src)
	svc.SetDisabled(func() (map[uint]bool, error) { return map[uint]bool{1: true}, nil })
	_, err := svc.Info(context.Background(), root, 1)
	apiErr(t, err, "instance_disabled")
	_, err = svc.Start(context.Background(), root, req(""))
	apiErr(t, err, "instance_disabled")
	if src.connects != 0 {
		t.Errorf("已停用的实例不能被连接: %d", src.connects)
	}
	// 查询失败时不阻断（旁路表不存在等）
	svc.SetDisabled(func() (map[uint]bool, error) { return nil, errors.New("no table") })
	if _, err := svc.Info(context.Background(), root, 1); err != nil {
		t.Errorf("%v", err)
	}
}
