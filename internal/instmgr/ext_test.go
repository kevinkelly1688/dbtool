// @author: kevinkelly
// @license: Apache Licence
// @file: ext_test.go
// @time: 2026/10/28

package instmgr

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dbtool/internal/instext"
	"dbtool/internal/model"
)

// ---- 停用 / 启用 / 环境 ----

func TestSetInstanceDisabledAndList(t *testing.T) {
	e, x := newExtEnv()
	if err := e.svc.SetInstanceDisabled(root, 1, true); err != nil {
		t.Fatal(err)
	}
	if !x.exts[1].Disabled || x.exts[1].DisabledBy != "root" {
		t.Fatalf("%+v", x.exts[1])
	}
	if a := e.repo.lastAudit(); a.Action != "停用实例" || !strings.Contains(a.ExtraInfo, "prod-mysql-01") {
		t.Fatalf("审计: %+v", a)
	}
	wantErr(t, e.svc.SetInstanceDisabled(root, 1, true), 409, "已经是停用")
	wantErr(t, e.svc.SetInstanceDisabled(root, 99, true), 404, "不存在")
	wantErr(t, e.svc.SetInstanceDisabled(bob, 2, true), 403, "未关联")

	res, err := e.svc.ListInstances(root, ListQuery{})
	if err != nil || !res.ExtReady || res.Stats == nil || res.Stats.Total != 5 || res.Stats.Disabled != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	var found bool
	for _, it := range res.Items {
		if it.ID == 1 {
			found = it.Disabled && it.DisabledBy == "root" && it.DisabledAt != ""
		} else if it.Disabled {
			t.Fatalf("只有 1 号被停用: %+v", it)
		}
	}
	if !found {
		t.Fatalf("列表应带停用信息: %+v", res.Items)
	}
	if r, _ := e.svc.ListInstances(root, ListQuery{State: "disabled"}); r.Total != 1 || r.Items[0].ID != 1 {
		t.Fatalf("按已停用筛选: %+v", r)
	}
	if r, _ := e.svc.ListInstances(root, ListQuery{State: "enabled"}); r.Total != 4 {
		t.Fatalf("按未停用筛选: %+v", r)
	}
	_, err = e.svc.ListInstances(root, ListQuery{State: "weird"})
	wantErr(t, err, 400, "状态")

	// 启用
	if err := e.svc.SetInstanceDisabled(root, 1, false); err != nil {
		t.Fatal(err)
	}
	if x.exts[1].Disabled || e.repo.lastAudit().Action != "启用实例" {
		t.Fatalf("%+v %+v", x.exts[1], e.repo.lastAudit())
	}
	wantErr(t, e.svc.SetInstanceDisabled(root, 1, false), 409, "已经是启用")
}

func TestDisabledInstanceIsNeverDialed(t *testing.T) {
	e, x := newExtEnv()
	x.exts[1] = instext.Row{InstanceID: 1, Disabled: true}
	_, err := e.svc.Processlist(context.Background(), root, 1, ProcFilter{})
	wantErr(t, err, 409, "已停用")
	_, err = e.svc.ListDatabases(context.Background(), root, 1, false, "")
	wantErr(t, err, 409, "已停用")
	if len(e.dialer.dialed) != 0 {
		t.Fatal("已停用的实例平台不再连接")
	}
	// 测试连接不受限制：停用前想先确认还连得通
	e.dialer.probeVer = "x"
	if _, err := e.svc.TestInstance(context.Background(), root, 3); err != nil {
		t.Fatal(err)
	}
	// 其他实例不受影响
	e.target.on("information_schema.processlist", rs(procCols))
	if _, err := e.svc.Processlist(context.Background(), root, 2, ProcFilter{}); err != nil {
		t.Fatal(err)
	}
}

func TestDisableNeedsExtTable(t *testing.T) {
	e, x := newExtEnv()
	x.instReady = false
	wantErr(t, e.svc.SetInstanceDisabled(root, 1, true), 409, "070_instance_ext.sql")
	res, err := e.svc.ListInstances(root, ListQuery{})
	if err != nil || res.ExtReady || len(res.Items) != 5 {
		t.Fatalf("表没建时列表照常工作: %+v %v", res, err)
	}
	// 旧仓库（没实现 ExtRepo）同样降级
	e2 := newEnv()
	wantErr(t, e2.svc.SetInstanceDisabled(root, 1, true), 409, "070_instance_ext.sql")
	// 扩展信息读失败不能让主流程失败
	e3, x3 := newExtEnv()
	x3.failExt = true
	e3.target.on("information_schema.processlist", rs(procCols))
	if _, err := e3.svc.Processlist(context.Background(), root, 1, ProcFilter{}); err != nil {
		t.Fatalf("读扩展失败应按“没有记录”处理: %v", err)
	}
}

func TestSaveInstanceEnvAndOptionalGroup(t *testing.T) {
	e, x := newExtEnv()
	in := goodInput()
	prod := "prod"
	in.Env = &prod
	it, err := e.svc.SaveInstance(root, 0, in)
	if err != nil {
		t.Fatal(err)
	}
	if it.Env != "prod" || it.EnvLabel != "生产" || len(x.envSets) != 1 {
		t.Fatalf("%+v %v", it, x.envSets)
	}
	if a := e.repo.lastAudit(); !strings.Contains(a.ExtraInfo, "环境：未设置 -> 生产") {
		t.Fatalf("审计应记环境变化: %s", a.ExtraInfo)
	}
	// 不带 env（旧客户端）不碰环境
	in2 := goodInput()
	in2.Name, in2.Password = "prod-mysql-01", ""
	if _, err := e.svc.SaveInstance(root, 1, in2); err != nil {
		t.Fatal(err)
	}
	if len(x.envSets) != 1 {
		t.Fatalf("env 省略不应写旁路表: %v", x.envSets)
	}
	// 非法环境
	bad1 := "dev"
	in3 := goodInput()
	in3.Name, in3.Env = "other", &bad1
	_, err = e.svc.SaveInstance(root, 0, in3)
	wantErr(t, err, 400, "环境")
	// 旁路表没建：设置环境被明确拒绝，而不是悄悄丢掉
	x.instReady = false
	in4 := goodInput()
	in4.Name, in4.Env = "other2", &prod
	_, err = e.svc.SaveInstance(root, 0, in4)
	wantErr(t, err, 409, "070_instance_ext.sql")
	// 资源组可以不选：新建、编辑都能保存，写给仓库的资源组列表为空
	in5 := goodInput()
	in5.Name, in5.GroupIDs = "no-group", nil
	if _, err = e.svc.SaveInstance(root, 0, in5); err != nil {
		t.Fatalf("不选资源组应能新建: %v", err)
	}
	if w := e.repo.writes[len(e.repo.writes)-1]; len(w.GroupIDs) != 0 || w.Name != "no-group" {
		t.Errorf("应写入空资源组: %+v", w)
	}
	in6 := goodInput()
	in6.Name, in6.Password, in6.GroupIDs = "prod-mysql-01", "", []uint{}
	if _, err = e.svc.SaveInstance(root, 1, in6); err != nil {
		t.Fatalf("清空资源组应能保存: %v", err)
	}
	if w := e.repo.writes[len(e.repo.writes)-1]; len(w.GroupIDs) != 0 {
		t.Errorf("应写入空资源组: %+v", w)
	}
	// 不存在的资源组仍然拒绝
	in7 := goodInput()
	in7.Name, in7.GroupIDs = "bad-group", []uint{9999}
	_, err = e.svc.SaveInstance(root, 0, in7)
	wantErr(t, err, 400, "资源组不存在")
}

func TestListInstancesFilterByEnv(t *testing.T) {
	e, x := newExtEnv()
	x.exts[1] = instext.Row{InstanceID: 1, Env: "prod"}
	x.exts[2] = instext.Row{InstanceID: 2, Env: "test"}
	if r, _ := e.svc.ListInstances(root, ListQuery{Env: "prod"}); r.Total != 1 || r.Items[0].ID != 1 {
		t.Fatalf("%+v", r)
	}
	_, err := e.svc.ListInstances(root, ListQuery{Env: "x"})
	wantErr(t, err, 400, "环境")
}

// ---- 健康探测 ----

func healthEnv() (*env, *extFakeRepo, *time.Time) {
	e, x := newExtEnv()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	e.svc.now = func() time.Time { return now }
	e.target.on("SHOW GLOBAL STATUS", rs([]string{"variable_name", "value"},
		[]any{"Questions", "6000"}, []any{"Threads_connected", "30"}, []any{"Threads_running", "4"}, []any{"Uptime", "600"}))
	e.target.on("SELECT VERSION()", rs([]string{"v"}, []any{"8.0.36"}))
	e.target.on("max_connections", rs([]string{"variable_name", "value"}, []any{"max_connections", "1000"}))
	return e, x, &now
}

func TestHealthOKQPSAndCache(t *testing.T) {
	e, _, now := healthEnv()
	h, err := e.svc.Health(context.Background(), root, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != HealthOK || h.Version != "8.0.36" || h.Connections != 30 || h.MaxConnections != 1000 || h.ThreadsRunning != 4 || h.QPS != 10 || h.QPSKind != "avg" {
		t.Fatalf("%+v", h)
	}
	// 缓存期内不再连库
	h2, _ := e.svc.Health(context.Background(), root, 1, false)
	if !h2.Cached || len(e.dialer.dialed) != 1 {
		t.Fatalf("应命中缓存: %+v dialed=%d", h2, len(e.dialer.dialed))
	}
	// 过期后重新探测，并按两次采样之差算 QPS
	*now = now.Add(time.Minute)
	e.target.handlers = e.target.handlers[:0]
	e.target.on("SHOW GLOBAL STATUS", rs([]string{"variable_name", "value"},
		[]any{"Questions", "9000"}, []any{"Threads_connected", "30"}, []any{"Threads_running", "4"}, []any{"Uptime", "660"}))
	h3, _ := e.svc.Health(context.Background(), root, 1, false)
	if h3.Cached || h3.QPSKind != "delta" || h3.QPS != 50 {
		t.Fatalf("两次采样 (9000-6000)/(660-600)=50: %+v", h3)
	}
	// force 忽略缓存
	n := len(e.dialer.dialed)
	if _, err := e.svc.Health(context.Background(), root, 1, true); err != nil || len(e.dialer.dialed) != n+1 {
		t.Fatalf("force 应重新探测: %v", err)
	}
}

func TestHealthWarnUnknownDisabled(t *testing.T) {
	e, x, _ := healthEnv()
	e.target.handlers = e.target.handlers[:0]
	e.target.on("SHOW GLOBAL STATUS", rs([]string{"variable_name", "value"},
		[]any{"Questions", "10"}, []any{"Threads_connected", "850"}, []any{"Threads_running", "4"}, []any{"Uptime", "10"}))
	e.target.on("max_connections", rs([]string{"variable_name", "value"}, []any{"max_connections", "1000"}))
	h, _ := e.svc.Health(context.Background(), root, 1, false)
	if h.Status != HealthWarn || !strings.Contains(h.Message, "85%") {
		t.Fatalf("连接使用率 85%% 应告警: %+v", h)
	}
	// 连不上：未知，不是错误
	e.dialer.dialErr = errors.New("dial tcp: i/o timeout")
	h, err := e.svc.Health(context.Background(), root, 2, false)
	if err != nil || h.Status != HealthUnknown || !strings.Contains(h.Message, "i/o timeout") {
		t.Fatalf("%+v %v", h, err)
	}
	// 读状态失败：未知
	e.dialer.dialErr = nil
	e.target.handlers = e.target.handlers[:0]
	e.target.onErr("SHOW GLOBAL STATUS", errors.New("denied"))
	h, _ = e.svc.Health(context.Background(), root, 5, false)
	if h.Status != HealthUnknown || !strings.Contains(h.Message, "denied") {
		t.Fatalf("%+v", h)
	}
	// 非 MySQL 系：走 Probe；不支持探测的类型显示未知
	e.dialer.probeVer = "SQL Server 2019"
	if h, _ := e.svc.Health(context.Background(), root, 3, false); h.Status != HealthOK || h.Version != "SQL Server 2019" {
		t.Fatalf("%+v", h)
	}
	e.dialer.probeVer, e.dialer.probeErr = "", errors.New("暂不支持 x 类型")
	if h, _ := e.svc.Health(context.Background(), root, 3, true); h.Status != HealthUnknown || !strings.Contains(h.Message, "暂不支持探测") {
		t.Fatalf("%+v", h)
	}
	// 已停用：不连接
	x.exts[2] = instext.Row{InstanceID: 2, Disabled: true}
	n := len(e.dialer.dialed)
	if h, _ := e.svc.Health(context.Background(), root, 2, true); h.Status != HealthDisabled || len(e.dialer.dialed) != n {
		t.Fatalf("%+v", h)
	}
	// 可见范围
	_, err = e.svc.Health(context.Background(), bob, 2, false)
	wantErr(t, err, 403, "未关联")
}

func TestHealthQueueTimeoutShowsUnknown(t *testing.T) {
	e, _, _ := healthEnv()
	e.svc.hc.queueFor = 20 * time.Millisecond
	for i := 0; i < cap(e.svc.hc.sem); i++ { // 名额被占满：模拟一大批实例同时在探测
		e.svc.hc.sem <- struct{}{}
	}
	h, err := e.svc.Health(context.Background(), root, 1, false)
	if err != nil || h.Status != HealthUnknown || !strings.Contains(h.Message, "排队") {
		t.Fatalf("排队超时应显示未知而不是卡住: %+v %v", h, err)
	}
	if len(e.dialer.dialed) != 0 {
		t.Fatal("没拿到名额不能连库")
	}
	<-e.svc.hc.sem // 释放一个，立刻能探测
	h, _ = e.svc.Health(context.Background(), root, 1, false)
	if h.Status != HealthOK {
		t.Fatalf("%+v", h)
	}
}

func TestComputeQPS(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cur := &qpsSample{questions: 1000, uptime: 100, at: t0.Add(time.Minute)}
	if q, k := computeQPS(nil, cur); q != 10 || k != "avg" {
		t.Fatalf("%v %s", q, k)
	}
	if q, k := computeQPS(&qpsSample{questions: 400, uptime: 40, at: t0}, cur); q != 10 || k != "delta" {
		t.Fatalf("%v %s", q, k)
	}
	// 实例重启过（计数器回退）/ 采样相隔太久：退回平均值
	if _, k := computeQPS(&qpsSample{questions: 5000, uptime: 40, at: t0}, cur); k != "avg" {
		t.Fatal(k)
	}
	if _, k := computeQPS(&qpsSample{questions: 400, uptime: 40, at: t0.Add(-time.Hour)}, cur); k != "avg" {
		t.Fatal(k)
	}
	if q, k := computeQPS(nil, &qpsSample{}); q != 0 || k != "" {
		t.Fatalf("%v %s", q, k)
	}
}

// ---- Binlog：从库进度 ----

func replicaEnv() (*env, *model.Instance) {
	e := newEnv()
	sl := e.repo.addInstance(6, "slave-01", "mysql")
	sl.Type, sl.Host = "slave", "10.0.0.9"
	e.target.onFunc("SELECT host AS host FROM information_schema.processlist", func(string, []any) (*Result, error) {
		return rs([]string{"host"}, []any{"10.0.0.9:51210"}, []any{"10.0.0.77:40000"}), nil
	})
	e.target.on("SHOW BINARY LOGS", rs([]string{"log_name", "file_size", "encrypted"},
		[]any{"mysql-bin.000812", "1073741824", "No"}, []any{"mysql-bin.000813", "1073741824", "No"}, []any{"mysql-bin.000814", "641728512", "No"}))
	e.target.on("Binlog Dump", rs([]string{"n"}, []any{int64(2)}))
	e.target.on("SHOW REPLICA STATUS", rs([]string{"source_host", "source_port", "source_log_file", "read_source_log_pos", "replica_io_running", "replica_sql_running"},
		[]any{"10.0.0.1", "3306", "mysql-bin.000813", "512", "Yes", "Yes"}))
	return e, sl
}

func TestBinlogPurgeRefusedBeyondReplicaProgress(t *testing.T) {
	e, _ := replicaEnv()
	// 从库正读 000813：清理到 000814 会删掉它没读完的 000813
	p, err := e.svc.PreviewPurge(context.Background(), root, 1, "mysql-bin.000814")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Blocked || !strings.Contains(p.BlockReason, "slave-01") || !strings.Contains(p.BlockReason, "mysql-bin.000813") {
		t.Fatalf("应判定越过从库进度: %+v", p)
	}
	var reg, unreg *ReplicaInfo
	for i := range p.Replicas {
		if p.Replicas[i].Registered {
			reg = &p.Replicas[i]
		} else {
			unreg = &p.Replicas[i]
		}
	}
	if reg == nil || !reg.Checked || reg.ReadFile != "mysql-bin.000813" || reg.ReadPos != 512 || !reg.Behind || reg.Name != "slave-01" {
		t.Fatalf("已登记从库的读取位置: %+v", p.Replicas)
	}
	if unreg == nil || unreg.Checked || unreg.Host != "10.0.0.77" {
		t.Fatalf("未登记的从库只能提示: %+v", p.Replicas)
	}
	if len(p.Warnings) == 0 || !strings.Contains(strings.Join(p.Warnings, ""), "10.0.0.77") {
		t.Fatalf("应提示读不到进度的从库: %v", p.Warnings)
	}
	// 执行时同样拒绝，且不发 PURGE，留审计
	_, err = e.svc.PurgeBinlogs(context.Background(), root, 1, "mysql-bin.000814", "mysql-bin.000814")
	wantErr(t, err, 409, "从库尚未读完")
	if len(e.target.execs()) != 0 {
		t.Fatal("被拒绝时不能执行 PURGE")
	}
	if a := e.repo.lastAudit(); a.Action != "清理Binlog（拒绝）" || !strings.Contains(a.ExtraInfo, "原因") {
		t.Fatalf("%+v", a)
	}
	// 清理到从库正在读的文件（它本身保留）是安全的
	p, err = e.svc.PurgeBinlogs(context.Background(), root, 1, "mysql-bin.000813", "mysql-bin.000813")
	if err != nil || p.Blocked || len(e.target.execs()) != 1 {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestBinlogReplicaLegacyStatusAndFailures(t *testing.T) {
	e, _ := replicaEnv()
	// 老版本：SHOW REPLICA STATUS 不认，回退 SHOW SLAVE STATUS
	e.target.handlers = e.target.handlers[:0]
	e.target.onFunc("SELECT host AS host FROM information_schema.processlist", func(string, []any) (*Result, error) {
		return rs([]string{"host"}, []any{"10.0.0.9:51210"}), nil
	})
	e.target.on("SHOW BINARY LOGS", rs([]string{"log_name", "file_size"}, []any{"mysql-bin.000812", "1"}, []any{"mysql-bin.000813", "1"}, []any{"mysql-bin.000814", "1"}))
	e.target.onErr("SHOW REPLICA STATUS", errors.New("You have an error in your SQL syntax"))
	e.target.on("SHOW SLAVE STATUS", rs([]string{"master_host", "master_port", "master_log_file", "read_master_log_pos", "slave_io_running", "slave_sql_running"},
		[]any{"10.0.0.1", "3306", "mysql-bin.000814", "99", "Yes", "Yes"}))
	p, err := e.svc.PreviewPurge(context.Background(), root, 1, "mysql-bin.000814")
	if err != nil || p.Blocked || len(p.Replicas) != 1 || !p.Replicas[0].Checked || p.Replicas[0].ReadFile != "mysql-bin.000814" {
		t.Fatalf("从库已读到 000814，清理到 000814 安全: %+v %v", p, err)
	}
	// 从库连不上：不阻止，但要提示
	e.dialer.failHost = map[string]error{"10.0.0.9": errors.New("connection refused")}
	p, err = e.svc.PreviewPurge(context.Background(), root, 1, "mysql-bin.000814")
	if err != nil {
		t.Fatal(err)
	}
	if p.Blocked || len(p.Replicas) != 1 || p.Replicas[0].Checked || !strings.Contains(strings.Join(p.Warnings, ""), "connection refused") {
		t.Fatalf("连不上从库应提示而不是静默通过: %+v", p)
	}
}

func TestHostOfDump(t *testing.T) {
	for in, want := range map[string]string{"10.0.0.9:51210": "10.0.0.9", "Db-Slave:3306": "db-slave", "[::1]:3306": "::1", "10.0.0.9": "10.0.0.9", "": ""} {
		if got := hostOfDump(in); got != want {
			t.Errorf("%q -> %q，期望 %q", in, got, want)
		}
	}
}

// ---- 会话 ----

func TestProcesslistLongRunningBlockerAndStats(t *testing.T) {
	e := procEnv()
	e.target.on("innodb_lock_waits", rs([]string{"wait_state", "wait_started", "wait_secs", "wait_thread", "wait_sql", "wait_object", "block_thread", "block_sql", "block_state", "block_started", "block_secs"},
		[]any{"LOCK WAIT", "2026-10-06 11:00:00", int64(35), int64(14), "update orders set x=1", "shop.orders", int64(13), "select ... for update", "RUNNING", "2026-10-06 10:30:00", int64(1832)}))
	e.target.on("information_schema.PROCESSLIST", rs([]string{"total", "active", "long_n"}, []any{int64(5), int64(2), int64(1)}))
	res, err := e.svc.Processlist(context.Background(), root, 1, ProcFilter{})
	if err != nil {
		t.Fatal(err)
	}
	by := map[int64]ProcRow{}
	for _, r := range res.Rows {
		by[r.ID] = r
	}
	if r := by[13]; !r.LongRunning || r.Severity != "crit" || !r.Blocker {
		t.Fatalf("1832 秒的查询应是长时间运行（>=1200 标红）且是阻塞源: %+v", r)
	}
	if r := by[14]; r.LongRunning || r.Blocker { // Sleep 不算长时间运行
		t.Fatalf("%+v", r)
	}
	if r := by[11]; r.LongRunning { // 受保护的复制线程不算
		t.Fatalf("%+v", r)
	}
	if res.Stats.Total != 5 || res.Stats.Active != 2 || res.Stats.Long != 1 || res.Stats.Blockers != 1 {
		t.Fatalf("%+v", res.Stats)
	}
	// 统计查询读不到：用返回的行兜底
	e2 := procEnv()
	r2, _ := e2.svc.Processlist(context.Background(), root, 1, ProcFilter{})
	if r2.Stats.Total != 5 || r2.Stats.Long != 1 || r2.Stats.Blockers != 0 {
		t.Fatalf("%+v", r2.Stats)
	}
}

func TestProcRowSeverityBoundaries(t *testing.T) {
	mk := func(cmd string, sec int64, user string) ProcRow {
		return procRowOf(Record{"id": int64(50), "user": user, "host": "h", "db": "", "command": cmd, "time": sec, "state": "", "info": ""}, 1)
	}
	if r := mk("Query", 299, "u"); r.LongRunning {
		t.Fatalf("%+v", r)
	}
	if r := mk("Query", 300, "u"); !r.LongRunning || r.Severity != "warn" {
		t.Fatalf("%+v", r)
	}
	if r := mk("Query", 1199, "u"); r.Severity != "warn" {
		t.Fatalf("%+v", r)
	}
	if r := mk("Query", 1200, "u"); r.Severity != "crit" {
		t.Fatalf("%+v", r)
	}
	if r := mk("Sleep", 5000, "u"); r.LongRunning {
		t.Fatalf("休眠连接不算: %+v", r)
	}
	if r := mk("Query", 5000, "event_scheduler"); r.LongRunning {
		t.Fatalf("受保护线程不算: %+v", r)
	}
}

func TestBuildLockChains(t *testing.T) {
	if len(BuildLockChains(nil)) != 0 {
		t.Fatal("没有锁等待应为空")
	}
	rows := []LockRow{
		{WaitThread: 21, WaitSQL: "update a", WaitSecs: 35, WaitObject: "shop.t1", BlockThread: 20, BlockSQL: "select for update", BlockSecs: 600},
		{WaitThread: 22, WaitSQL: "update b", WaitSecs: 21, WaitObject: "shop.t1", BlockThread: 21},
		{WaitThread: 31, WaitSQL: "update c", WaitSecs: 3, WaitObject: "shop.t2", BlockThread: 30, BlockSQL: ""},
		{WaitThread: 23, WaitSQL: "update d", WaitSecs: 9, WaitObject: "shop.t1", BlockThread: 20},
	}
	ch := BuildLockChains(rows)
	if len(ch) != 2 || ch[0].Root != 20 || ch[1].Root != 30 {
		t.Fatalf("两条链，根因 20 与 30: %+v", ch)
	}
	var got []string
	for _, n := range ch[0].Nodes {
		got = append(got, itoa(uint(n.Thread))+":"+itoa(uint(n.Depth))+":"+n.Role)
	}
	if strings.Join(got, ",") != "20:0:root,21:1:blocked,22:2:blocked,23:1:blocked" {
		t.Fatalf("深度优先、层级正确: %v", got)
	}
	if n := ch[0].Nodes[0]; n.SQL != "select for update" || n.Secs != 600 {
		t.Fatalf("根因带 SQL 与事务时长: %+v", n)
	}
	if n := ch[0].Nodes[2]; n.SQL != "update b" || n.Secs != 21 || n.Object != "shop.t1" {
		t.Fatalf("被阻塞者带等待信息: %+v", n)
	}
	// 环：每个线程只出现一次，不死循环
	cyc := BuildLockChains([]LockRow{{WaitThread: 1, BlockThread: 2}, {WaitThread: 2, BlockThread: 1}})
	if len(cyc) != 1 || len(cyc[0].Nodes) != 2 {
		t.Fatalf("%+v", cyc)
	}
}

func TestLockWaitsWithChainsEndpointData(t *testing.T) {
	e := newEnv().withVersion("5.7.40")
	e.target.on("innodb_lock_waits", rs([]string{"wait_state", "wait_started", "wait_secs", "wait_thread", "wait_sql", "wait_object", "block_thread", "block_sql", "block_state", "block_started", "block_secs"},
		[]any{"LOCK WAIT", "t", int64(3), int64(8), "u1", "o", int64(7), "b", "RUNNING", "t", int64(30)}))
	v, err := e.svc.LockWaitsWithChains(context.Background(), root, 1)
	if err != nil || len(v.Rows) != 1 || len(v.Chains) != 1 || v.Chains[0].Root != 7 || len(v.Chains[0].Nodes) != 2 {
		t.Fatalf("%+v %v", v, err)
	}
}

// ---- 数据库管理 ----

func TestListDatabasesSizesAndScope(t *testing.T) {
	e := newEnv()
	e.target.on("information_schema.schemata", rs([]string{"schema_name", "db_charset", "db_collation"},
		[]any{"shop", "utf8mb4", "utf8mb4_general_ci"}, []any{"tmp_x", "utf8", "utf8_general_ci"}))
	e.target.on("GROUP BY table_schema", rs([]string{"db", "n", "bytes"}, []any{"shop", int64(12), float64(5 * 1024 * 1024)}))
	e.repo.dbs[1] = map[string]DatabaseRecord{"shop": {DBName: "shop", Owner: "dev", OwnerDisplay: "演示开发者"}}
	all, err := e.svc.ListDatabasesScoped(context.Background(), root, 1, "", "")
	if err != nil || len(all) != 2 {
		t.Fatalf("%+v %v", all, err)
	}
	if all[0].Name != "shop" || all[0].Tables != 12 || all[0].SizeMB != 5 || !all[0].StatsOK {
		t.Fatalf("表数 / 大小: %+v", all[0])
	}
	if all[1].StatsOK || all[1].Tables != 0 {
		t.Fatalf("读不到的库显示空: %+v", all[1])
	}
	if l, _ := e.svc.ListDatabasesScoped(context.Background(), root, 1, "0", ""); len(l) != 1 || l[0].Name != "tmp_x" {
		t.Fatalf("只看未录入: %+v", l)
	}
	if l, _ := e.svc.ListDatabasesScoped(context.Background(), root, 1, "1", ""); len(l) != 1 || l[0].Name != "shop" {
		t.Fatalf("只看已录入: %+v", l)
	}
	_, err = e.svc.ListDatabasesScoped(context.Background(), root, 1, "2", "")
	wantErr(t, err, 400, "范围")
	// 大小统计失败不影响列表
	e2 := newEnv()
	e2.target.on("information_schema.schemata", rs([]string{"schema_name", "db_charset", "db_collation"}, []any{"shop", "utf8mb4", "x"}))
	e2.target.onErr("GROUP BY table_schema", errors.New("denied"))
	if l, err := e2.svc.ListDatabasesScoped(context.Background(), root, 1, "", ""); err != nil || len(l) != 1 || l[0].StatsOK {
		t.Fatalf("%+v %v", l, err)
	}
}

// ---- 账号管理 ----

func TestListAccountsFiltersCountsAndRegistration(t *testing.T) {
	e, x := newExtEnv()
	e.withVersion("8.0.36")
	e.target.on("CURRENT_USER()", rs([]string{"cu"}, []any{"dba_ro@10.0.0.2"}))
	e.target.on("ORDER BY user, host", rs([]string{"u", "h", "locked"},
		[]any{"app_rw", "10.20.%", "N"}, []any{"dba_ro", "10.0.0.2", "N"}, []any{"mysql.sys", "localhost", "Y"}, []any{"root", "%", "N"}))
	e.target.onFunc("SHOW GRANTS FOR", func(_ string, args []any) (*Result, error) {
		if args[0] == "root" {
			return rs([]string{"g"}, []any{"GRANT ALL PRIVILEGES ON *.* TO `root`@`%` WITH GRANT OPTION"}), nil
		}
		return rs([]string{"g"}, []any{"GRANT USAGE ON *.* TO `x`@`y`"}), nil
	})
	e.repo.accts[1] = map[string]AccountRecord{"app_rw@10.20.%": {User: "app_rw", Host: "10.20.%", Remark: "r"}}
	x.regs[1] = map[string]AccountReg{"app_rw@10.20.%": {User: "app_rw", Host: "10.20.%", Owner: "dev", OwnerDisplay: "演示开发者", Purpose: "商城读写", ExpireDate: "2026-10-09"}}

	all, err := e.svc.ListAccountsFiltered(context.Background(), root, 1, AccountFilter{})
	if err != nil || len(all.Items) != 4 || all.Total != 4 || all.Risky != 1 || all.Locked != 1 || all.Unsaved != 3 || !all.RegReady {
		t.Fatalf("%+v %v", all, err)
	}
	var app AccountItem
	for _, a := range all.Items {
		if a.User == "app_rw" {
			app = a
		}
	}
	if app.Owner != "dev" || app.Purpose != "商城读写" || app.ExpireDate != "2026-10-09" || app.ExpireState != "soon" || app.ExpireDays != 3 {
		t.Fatalf("登记信息应并入账号: %+v", app)
	}
	if l, _ := e.svc.ListAccountsFiltered(context.Background(), root, 1, AccountFilter{Saved: "0"}); len(l.Items) != 3 || l.Total != 4 {
		t.Fatalf("未录入 3 个，总览不受状态筛选影响: %+v", l)
	}
	if l, _ := e.svc.ListAccountsFiltered(context.Background(), root, 1, AccountFilter{Risk: true}); len(l.Items) != 1 || l.Items[0].User != "root" {
		t.Fatalf("高危: %+v", l.Items)
	}
	if l, _ := e.svc.ListAccountsFiltered(context.Background(), root, 1, AccountFilter{Locked: true}); len(l.Items) != 1 || l.Items[0].User != "mysql.sys" {
		t.Fatalf("已锁定: %+v", l.Items)
	}
	_, err = e.svc.ListAccountsFiltered(context.Background(), root, 1, AccountFilter{Saved: "x"})
	wantErr(t, err, 400, "范围")
	// 旧签名保持
	if l, _ := e.svc.ListAccounts(context.Background(), root, 1, true, ""); len(l.Items) != 1 {
		t.Fatalf("%+v", l)
	}
}

func TestExpireState(t *testing.T) {
	now := time.Date(2026, 10, 6, 15, 30, 0, 0, time.Local)
	cases := []struct {
		in    string
		state string
		days  int
	}{{"", "", 0}, {"2026-10-05", "expired", -1}, {"2026-10-06", "soon", 0}, {"2026-10-13", "soon", 7}, {"2026-10-14", "", 8}, {"bad", "", 0}}
	for _, c := range cases {
		st, d := expireState(c.in, now)
		if st != c.state || d != c.days {
			t.Errorf("%q -> %s/%d，期望 %s/%d", c.in, st, d, c.state, c.days)
		}
	}
}

func TestSaveAccountInfoRegistration(t *testing.T) {
	e, x := newExtEnv()
	owner, purpose, exp := "dev", "商城读写", "2026-12-31"
	if err := e.svc.SaveAccountInfo(root, 1, AccountInfo{User: "app_rw", Host: "10.20.%", Remark: "r", Owner: &owner, Purpose: &purpose, ExpireDate: &exp}); err != nil {
		t.Fatal(err)
	}
	g := x.regs[1]["app_rw@10.20.%"]
	if g.Owner != "dev" || g.OwnerDisplay != "演示开发者" || g.Purpose != "商城读写" || g.ExpireDate != "2026-12-31" {
		t.Fatalf("%+v", g)
	}
	if a := e.repo.lastAudit(); !strings.Contains(a.ExtraInfo, "使用人：dev") || !strings.Contains(a.ExtraInfo, "2026-12-31") {
		t.Fatalf("审计: %s", a.ExtraInfo)
	}
	// 省略（nil）的字段保持原值
	p2 := "新用途"
	if err := e.svc.SaveAccountInfo(root, 1, AccountInfo{User: "app_rw", Host: "10.20.%", Purpose: &p2}); err != nil {
		t.Fatal(err)
	}
	g = x.regs[1]["app_rw@10.20.%"]
	if g.Owner != "dev" || g.Purpose != "新用途" || g.ExpireDate != "2026-12-31" {
		t.Fatalf("%+v", g)
	}
	// 完全不带这三项（旧客户端）：不碰登记
	if err := e.svc.SaveAccountInfo(root, 1, AccountInfo{User: "app_rw", Host: "10.20.%", Remark: "x"}); err != nil || x.regs[1]["app_rw@10.20.%"].Purpose != "新用途" {
		t.Fatalf("%v %+v", err, x.regs[1])
	}
	// 校验
	badUser, badDate := "nobody", "2026/12/31"
	wantErr(t, e.svc.SaveAccountInfo(root, 1, AccountInfo{User: "a", Host: "h", Owner: &badUser}), 400, "负责人不存在")
	wantErr(t, e.svc.SaveAccountInfo(root, 1, AccountInfo{User: "a", Host: "h", ExpireDate: &badDate}), 400, "YYYY-MM-DD")
	// 全部清空 = 清除登记
	empty := ""
	if err := e.svc.SaveAccountInfo(root, 1, AccountInfo{User: "app_rw", Host: "10.20.%", Owner: &empty, Purpose: &empty, ExpireDate: &empty}); err != nil {
		t.Fatal(err)
	}
	if _, ok := x.regs[1]["app_rw@10.20.%"]; ok {
		t.Fatal("应清除")
	}
	// 旁路表没建：要写非空登记就明确拒绝；清空则无妨
	x.regReady = false
	wantErr(t, e.svc.SaveAccountInfo(root, 1, AccountInfo{User: "a", Host: "h", Owner: &owner}), 409, "071_account_reg.sql")
	if err := e.svc.SaveAccountInfo(root, 1, AccountInfo{User: "a", Host: "h", Remark: "ok", Owner: &empty}); err != nil {
		t.Fatalf("表没建时备注照常保存: %v", err)
	}
}

func TestCreateAccountsWithRegistrationAndDrop(t *testing.T) {
	e, x := newExtEnv()
	e.withVersion("8.0.36")
	if _, err := e.svc.CreateAccounts(context.Background(), root, 1, AccountCreate{User: "etl", Host: "10.20.%|10.30.%", Password: "Passw0rd!x", Password2: "Passw0rd!x",
		Owner: "wang", Purpose: "ETL", ExpireDate: "2027-01-01", Confirm: true}); err != nil {
		t.Fatal(err)
	}
	if x.regs[1]["etl@10.20.%"].Owner != "wang" || x.regs[1]["etl@10.30.%"].ExpireDate != "2027-01-01" {
		t.Fatalf("每个主机一条登记: %+v", x.regs[1])
	}
	_, err := e.svc.CreateAccounts(context.Background(), root, 1, AccountCreate{User: "etl2", Host: "10.20.%", Password: "Passw0rd!x", Password2: "Passw0rd!x", Owner: "ghost", Confirm: true})
	wantErr(t, err, 400, "负责人不存在")
	for _, ex := range e.target.execs() {
		if strings.Contains(ex.sql, "etl2") {
			t.Fatal("校验失败不能先建账号")
		}
	}
	// 删除账号时一并清登记
	e.target.onFunc("WHERE user = ? AND host = ?", func(string, []any) (*Result, error) { return rs([]string{"n"}, []any{int64(1)}), nil })
	e.target.on("CURRENT_USER()", rs([]string{"cu"}, []any{"dba_ro@10.0.0.2"}))
	if err := e.svc.DropAccount(context.Background(), root, 1, DropReq{User: "etl", Host: "10.20.%", Confirm: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := x.regs[1]["etl@10.20.%"]; ok {
		t.Fatal("登记应一并清理")
	}
}

func TestBuildGrantAllPrivileges(t *testing.T) {
	plan, err := buildGrant(GrantReq{User: "u", Host: "h", Op: "grant", Scope: "db", DBs: []string{"shop"}, Privs: []string{"select", "all"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Statements) != 1 || plan.Statements[0] != "GRANT ALL PRIVILEGES ON `shop`.* TO 'u'@'h';" {
		t.Fatalf("选了 ALL 就只授 ALL PRIVILEGES: %v", plan.Statements)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "ALL PRIVILEGES") {
		t.Fatalf("要警告高危: %v", plan.Warnings)
	}
	plan, err = buildGrant(GrantReq{User: "u", Host: "h", Op: "grant", Scope: "global", Privs: []string{"ALL PRIVILEGES", "GRANT OPTION"}})
	if err != nil || plan.Statements[0] != "GRANT ALL PRIVILEGES, GRANT OPTION ON *.* TO 'u'@'h';" {
		t.Fatalf("%v %v", plan, err)
	}
	plan, err = buildGrant(GrantReq{User: "u", Host: "h", Op: "revoke", Scope: "db", DBs: []string{"shop"}, Privs: []string{"ALL PRIVILEGES"}})
	if err != nil || plan.Statements[0] != "REVOKE ALL PRIVILEGES ON `shop`.* FROM 'u'@'h';" {
		t.Fatalf("%v %v", plan, err)
	}
	for _, scope := range []string{"table", "column"} {
		_, err = buildGrant(GrantReq{User: "u", Host: "h", Op: "grant", Scope: scope, DBs: []string{"d"}, Tables: []string{"t"}, Columns: []string{"c"}, Privs: []string{"ALL PRIVILEGES"}})
		wantErr(t, err, 400, "不支持权限")
	}
}

// ---- 参数配置 ----

func paramExtEnv() (*env, *extFakeRepo) {
	e, x := newExtEnv()
	e.withVersion("8.0.36")
	e.repo.tpls = []ParamTemplate{
		{ID: 1, DBType: "mysql", VariableName: "max_connections", Editable: true, ValidValues: "[100-100000]"},
		{ID: 2, DBType: "mysql", VariableName: "slow_query_log", Editable: true, ValidValues: "[ON|OFF]"},
		{ID: 3, DBType: "mysql", VariableName: "init_connect", Editable: false},
		{ID: 4, DBType: "mysql", VariableName: "innodb_log_file_size", Editable: true},
	}
	x.rec = map[uint]string{1: "2000", 2: "ON", 4: "2147483648"}
	e.target.on("SHOW GLOBAL VARIABLES", rs([]string{"variable_name", "value"},
		[]any{"max_connections", "2000"}, []any{"slow_query_log", "OFF"}, []any{"init_connect", ""}, []any{"innodb_log_file_size", "50331648"}, []any{"version", "8.0.36"}))
	return e, x
}

func TestListParamsDeviationAndFlags(t *testing.T) {
	e, _ := paramExtEnv()
	rows, err := e.svc.ListParamsEx(context.Background(), root, 1, ParamFilter{})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]ParamRow{}
	for _, r := range rows {
		by[r.Name] = r
	}
	if r := by["max_connections"]; r.Recommended != "2000" || r.Deviates {
		t.Fatalf("与推荐一致: %+v", r)
	}
	if r := by["slow_query_log"]; r.Recommended != "ON" || !r.Deviates {
		t.Fatalf("OFF 偏离推荐 ON: %+v", r)
	}
	if r := by["innodb_log_file_size"]; !r.Deviates || !r.NeedsRestart {
		t.Fatalf("%+v", r)
	}
	if r := by["init_connect"]; !r.Blacklisted || r.Recommended != "" || r.Deviates {
		t.Fatalf("没有推荐值不算偏离: %+v", r)
	}
	if r := by["version"]; r.TemplateID != 0 || r.Deviates {
		t.Fatalf("不在模板里: %+v", r)
	}
	dev, _ := e.svc.ListParamsEx(context.Background(), root, 1, ParamFilter{Deviation: true})
	if len(dev) != 2 {
		t.Fatalf("只看偏离的 2 项: %+v", dev)
	}
	// 旧签名保持，且旁路表没建时没有推荐值
	e2, x2 := paramExtEnv()
	x2.tplReady = false
	old, _ := e2.svc.ListParams(context.Background(), root, 1, "all", "")
	for _, r := range old {
		if r.Recommended != "" || r.Deviates {
			t.Fatalf("表没建不应有推荐值: %+v", r)
		}
	}
}

func TestSameValue(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"ON", "on", true}, {"1", "ON", true}, {"0", "OFF", true}, {"OFF", "ON", false}, {"2000", "2000.0", true}, {"2000", "2001", false}, {"abc", "ABC", true}, {"", "x", false}} {
		if got := sameValue(c.a, c.b); got != c.want {
			t.Errorf("sameValue(%q,%q)=%v", c.a, c.b, got)
		}
	}
}

func TestSaveTemplateRecommended(t *testing.T) {
	e, x := paramExtEnv()
	rec := "4000"
	t1, err := e.svc.SaveTemplate(root, 1, TemplateInput{DefaultValue: "151", Editable: true, ValidValues: "[100-100000]", RecommendedValue: &rec})
	if err != nil || x.rec[t1.ID] != "4000" {
		t.Fatalf("%+v %v %v", t1, err, x.rec)
	}
	if !strings.Contains(e.repo.lastAudit().ExtraInfo, "推荐值：4000") {
		t.Fatalf("审计: %s", e.repo.lastAudit().ExtraInfo)
	}
	// 省略 = 不修改；空串 = 清除
	if _, err := e.svc.SaveTemplate(root, 1, TemplateInput{Editable: true, ValidValues: "[100-100000]"}); err != nil || x.rec[1] != "4000" {
		t.Fatalf("%v %v", err, x.rec)
	}
	empty := ""
	if _, err := e.svc.SaveTemplate(root, 1, TemplateInput{Editable: true, ValidValues: "[100-100000]", RecommendedValue: &empty}); err != nil {
		t.Fatal(err)
	}
	if _, ok := x.rec[1]; ok {
		t.Fatal("应清除")
	}
	// 推荐值要满足可修改值
	out := "5"
	_, err = e.svc.SaveTemplate(root, 1, TemplateInput{Editable: true, ValidValues: "[100-100000]", RecommendedValue: &out})
	wantErr(t, err, 400, "推荐值")
	// 新建
	nv := "ON"
	nt, err := e.svc.SaveTemplate(root, 0, TemplateInput{DBType: "mysql", VariableName: "innodb_print_all_deadlocks", Editable: true, ValidValues: "[ON|OFF]", RecommendedValue: &nv})
	if err != nil || x.rec[nt.ID] != "ON" {
		t.Fatalf("%+v %v", nt, err)
	}
	// 旁路表没建：非空推荐值明确拒绝
	x.tplReady = false
	_, err = e.svc.SaveTemplate(root, 1, TemplateInput{Editable: true, ValidValues: "[100-100000]", RecommendedValue: &rec})
	wantErr(t, err, 409, "072_param_template_ext.sql")
}

func TestTemplateViews(t *testing.T) {
	e, _ := paramExtEnv()
	vs, err := e.svc.ListTemplateViews("mysql")
	if err != nil || len(vs) != 4 {
		t.Fatalf("%+v %v", vs, err)
	}
	by := map[string]TemplateView{}
	for _, v := range vs {
		by[v.VariableName] = v
	}
	if !by["init_connect"].Blacklisted || !by["innodb_log_file_size"].NeedsRestart || by["max_connections"].Recommended != "2000" {
		t.Fatalf("%+v", by)
	}
	_, err = e.svc.ListTemplateViews("mssql")
	wantErr(t, err, 400, "暂不支持")
}

func TestListParamChangesAll(t *testing.T) {
	e, x := newExtEnv()
	x.allHist = []ParamHistoryRow{
		{ParamHistory: ParamHistory{ID: 1, InstanceID: 1, VariableName: "a", CreateTime: time.Now()}, InstanceName: "prod-mysql-01"},
		{ParamHistory: ParamHistory{ID: 2, InstanceID: 2, VariableName: "b", CreateTime: time.Now()}, InstanceName: "prod-mysql-02"},
		{ParamHistory: ParamHistory{ID: 3, InstanceID: 3, VariableName: "c", CreateTime: time.Now()}, InstanceName: "mssql-01"},
	}
	res, err := e.svc.ListParamChangesAll(root, "", 1, 20)
	if err != nil || res.Total != 3 || res.Rows[0].ID != 3 || res.Rows[0].CreateTimeText == "" {
		t.Fatalf("超级用户看全部，新的在前: %+v %v", res, err)
	}
	res, _ = e.svc.ListParamChangesAll(bob, "", 1, 20) // bob 只关联 1、3、4
	if res.Total != 2 {
		t.Fatalf("普通用户只看可见实例: %+v", res)
	}
	if res, _ := e.svc.ListParamChangesAll(root, "prod-mysql-02", 1, 20); res.Total != 1 {
		t.Fatalf("可按实例名搜: %+v", res)
	}
	// 旧仓库不支持
	_, err = newEnv().svc.ListParamChangesAll(root, "", 1, 20)
	wantErr(t, err, 400, "不支持")
}

func TestExtRefsAreIndependentOfInstext(t *testing.T) {
	// instext.Reset 之后不应残留状态（其他测试依赖）
	instext.Reset()
}

// ---- HTTP ----

func TestExtEndpointsOverHTTP(t *testing.T) {
	e, x, _ := healthEnv()
	e.target.on("lock_waits", rs([]string{"wait_state", "wait_started", "wait_secs", "wait_thread", "wait_sql", "wait_object", "block_thread", "block_sql", "block_state", "block_started", "block_secs"},
		[]any{"LOCK WAIT", "t", int64(3), int64(8), "u1", "o", int64(7), "b", "RUNNING", "t", int64(30)}))
	x.allHist = []ParamHistoryRow{{ParamHistory: ParamHistory{ID: 1, InstanceID: 1, VariableName: "max_connections", CreateTime: time.Now()}, InstanceName: "prod-mysql-01"}}
	env := newHTTP(t, e)
	root := env.Login("root")

	var h Health
	root.GetJSON("/api/v1/instances/1/health", &h)
	if h.Status != HealthOK || h.Version != "8.0.36" || h.Connections != 30 {
		t.Fatalf("%+v", h)
	}
	if code, _ := root.JSON("GET", "/api/v1/instances/99/health", nil, nil); code != 404 {
		t.Fatalf("%d", code)
	}

	root.PostJSON("/api/v1/instances/1/disable", map[string]any{"disabled": true}, nil)
	if !x.exts[1].Disabled {
		t.Fatal("应已停用")
	}
	root.GetJSON("/api/v1/instances/1/health", &h)
	if h.Status != HealthDisabled {
		t.Fatalf("%+v", h)
	}
	var list InstanceList
	root.GetJSON("/api/v1/instances?state=disabled&env=", &list)
	if list.Total != 1 || !list.Items[0].Disabled || list.Stats == nil || list.Stats.Disabled != 1 {
		t.Fatalf("%+v", list)
	}
	if code, msg := root.JSON("POST", "/api/v1/instances/1/disable", map[string]any{"disabled": true}, nil); code != 409 || !strings.Contains(msg, "已经是停用") {
		t.Fatalf("%d %s", code, msg)
	}
	root.PostJSON("/api/v1/instances/1/disable", map[string]any{"disabled": false}, nil)

	// 锁等待：默认形态是数组，chains=1 是带阻塞链的对象
	var rows []LockRow
	root.GetJSON("/api/v1/instances/1/lock-waits", &rows)
	if len(rows) != 1 {
		t.Fatalf("%+v", rows)
	}
	var lv LockView
	root.GetJSON("/api/v1/instances/1/lock-waits?chains=1", &lv)
	if len(lv.Rows) != 1 || len(lv.Chains) != 1 || lv.Chains[0].Root != 7 {
		t.Fatalf("%+v", lv)
	}

	var hp HistoryPage
	root.GetJSON("/api/v1/params/history?q=max", &hp)
	if hp.Total != 1 || hp.Rows[0].InstanceName != "prod-mysql-01" {
		t.Fatalf("%+v", hp)
	}

	var caps map[string]bool
	root.GetJSON("/api/v1/instmgr/caps", &caps)
	if _, ok := caps["session.kill"]; !ok {
		t.Fatalf("caps 应包含 session.kill 按钮: %v", caps)
	}
}

func TestAccountOwners(t *testing.T) {
	e, _ := newExtEnv()
	list, err := e.svc.AccountOwners(root, 1)
	if err != nil || len(list) != 2 || list[0].Username != "dev" || list[1].Username != "wang" {
		t.Fatalf("按账号排序的启用用户: %+v %v", list, err)
	}
	_, err = e.svc.AccountOwners(bob, 2)
	wantErr(t, err, 403, "未关联")
	_, err = e.svc.AccountOwners(root, 99)
	wantErr(t, err, 404, "不存在")
}

func TestHealthStatusUnsupportedButConnected(t *testing.T) {
	e, _ := newExtEnv()
	e.target.onErr("SHOW GLOBAL STATUS", errors.New("Error 1064: syntax error near 'GLOBAL STATUS'"))
	e.target.on("SELECT VERSION()", rs([]string{"v"}, []any{"5.7.99-doris-2.0"}))
	h, err := e.svc.Health(context.Background(), root, 4, false) // doris
	if err != nil || h.Status != HealthOK || h.Version != "5.7.99-doris-2.0" || h.QPSKind != "" || !strings.Contains(h.Message, "只确认了连接和版本") {
		t.Fatalf("不支持 SHOW GLOBAL STATUS 但连得上：%+v %v", h, err)
	}
}
