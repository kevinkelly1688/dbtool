// @author: kevinkelly
// @license: Apache Licence
// @file: opener_test.go
// @time: 2026/10/28

package engine_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"dbtool/internal/archive/engine"
	eng "dbtool/internal/engine"
)

func openerFor(f *fakeMy, t *testing.T) (engine.Opener, eng.Conn) {
	c := f.conn(t)
	return engine.Opener{
		Conn: func(ctx context.Context, id uint) (eng.Conn, string, error) { return c, "prod-db01", nil },
	}, c
}

func fakeSpec() engine.Spec {
	return engine.Spec{
		TaskID: 1, RunID: 1, Name: "orders 归档", InstanceID: 1, DB: "shop", Table: "orders",
		TimeColumn: "created_at", RetentionDays: 90, Mode: engine.ModeMove,
		Dest:   &engine.TableDest{InstanceID: 1, DB: "shop_arch", Template: "{table}_bak"},
		Params: engine.Params{BatchSize: 100, SleepMs: 50, LockWaitSeconds: 5},
	}
}

func fakePolicy() engine.Policy { return engine.Policy{MinRetentionDays: 30} }

func TestOpenerSetsSessionParams(t *testing.T) {
	f := newFakeMy(10)
	op, _ := openerFor(f, t)
	env, err := op.Open(t.Context(), fakeSpec())
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	if env.Src == nil || env.Dst == nil || env.Probe == nil || env.Files != nil || env.InstName != "prod-db01" {
		t.Fatalf("Env：%+v", env)
	}
	for _, want := range []string{
		" lock_wait_timeout = 5",
		"innodb_lock_wait_timeout = 5",
		"time_zone = '+08:00'",
		"sql_mode = 'STRICT_TRANS_TABLES,NO_AUTO_VALUE_ON_ZERO'",
		"SELECT @@session.sql_log_bin",
		"SELECT TIMEDIFF(NOW(), UTC_TIMESTAMP())",
	} {
		if !f.saw(want) {
			t.Errorf("没有发出 %q；实际：%v", want, f.queries)
		}
	}
	// 默认锁等待 3 秒
	f2 := newFakeMy(1)
	op2, _ := openerFor(f2, t)
	s := fakeSpec()
	s.Params.LockWaitSeconds = 0
	env2, err := op2.Open(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	env2.Close()
	if !f2.saw(" lock_wait_timeout = 3") || !f2.saw("innodb_lock_wait_timeout = 3") {
		t.Errorf("默认应为 3 秒")
	}
}

// PolarDB-X：不认识 lock_wait_timeout / innodb_lock_wait_timeout，也读不到 @@session.sql_log_bin。
// 兼容实现（Lenient）降级继续；MySQL（没标 Lenient）仍然严格拒绝。
func TestOpenerLenientOnCompatibleEngines(t *testing.T) {
	failing := func(q string) error {
		switch {
		case strings.Contains(q, "lock_wait_timeout"):
			return errors.New("ERROR 1193 (HY000): Unknown system variable")
		case strings.HasPrefix(q, "SELECT @@session.sql_log_bin"):
			return errors.New("ERROR 1193 (HY000): Unknown system variable 'sql_log_bin'")
		}
		return nil
	}
	f := newFakeMy(10)
	f.failOn = failing
	op, _ := openerFor(f, t)
	op.Lenient = func(context.Context, uint) bool { return true }
	env, err := op.Open(t.Context(), fakeSpec())
	if err != nil {
		t.Fatalf("兼容实现应降级继续：%v", err)
	}
	defer env.Close()
	// 即使设不上，也尝试过（SET SESSION …），而且表结构能正常读取
	if !f.saw("SET SESSION lock_wait_timeout = 5") {
		t.Errorf("应尝试设置锁等待：%v", f.queries)
	}
	if info, err := env.Src.Describe(t.Context(), "shop", "orders"); err != nil || !info.Exists || len(info.PrimaryKey) != 1 {
		t.Errorf("Describe：%+v %v", info, err)
	}

	// 不标兼容：严格——不认识的会话变量让建连失败
	f2 := newFakeMy(10)
	f2.failOn = failing
	op2, _ := openerFor(f2, t)
	if env2, err := op2.Open(t.Context(), fakeSpec()); err == nil {
		env2.Close()
		t.Fatal("MySQL 上设不上锁等待参数必须失败，不能静默继续")
	}

	// 兼容实现读到 sql_log_bin=0 仍然拒绝（读不到才放过）
	f3 := newFakeMy(10)
	f3.logBin = "0"
	op3, _ := openerFor(f3, t)
	op3.Lenient = func(context.Context, uint) bool { return true }
	if env3, err := op3.Open(t.Context(), fakeSpec()); err == nil || !strings.Contains(err.Error(), "sql_log_bin") {
		if env3 != nil {
			env3.Close()
		}
		t.Fatalf("sql_log_bin=0 无论哪种实例都要拒绝：%v", err)
	}
}

// information_schema 的 COLUMNS / STATISTICS 不可用时，退到 SHOW COLUMNS / SHOW INDEX 仍能取到列和主键。
func TestDescribeFallsBackToShowStatements(t *testing.T) {
	f := newFakeMy(10)
	f.failOn = func(q string) error {
		if strings.Contains(q, "FROM information_schema.COLUMNS") || strings.Contains(q, "FROM information_schema.STATISTICS") {
			return errors.New("ERR-CODE: [TDDL-4506][ERR_NOT_SUPPORT] information_schema")
		}
		return nil
	}
	op, _ := openerFor(f, t)
	op.Lenient = func(context.Context, uint) bool { return true }
	env, err := op.Open(t.Context(), fakeSpec())
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	info, err := env.Src.Describe(t.Context(), "shop", "orders")
	if err != nil {
		t.Fatalf("应退到 SHOW 语句：%v", err)
	}
	if len(info.Columns) != 4 || info.Columns[0].Name != "id" || len(info.PrimaryKey) != 1 || info.PrimaryKey[0] != "id" || info.Fingerprint == "" {
		t.Errorf("列 / 主键：%+v", info)
	}
}

func TestOpenerRefusesSqlLogBinOff(t *testing.T) {
	f := newFakeMy(10)
	f.logBin = "0"
	op, _ := openerFor(f, t)
	env, err := op.Open(t.Context(), fakeSpec())
	if err == nil || env != nil || !strings.Contains(err.Error(), "sql_log_bin") {
		t.Fatalf("sql_log_bin=0 必须拒绝：%v %v", env, err)
	}
	// 目标端是另一个实例且它的 sql_log_bin=0
	src, dst := newFakeMy(10), newFakeMy(0)
	dst.logBin = "0"
	sc, dc := src.conn(t), dst.conn(t)
	op = engine.Opener{Conn: func(ctx context.Context, id uint) (eng.Conn, string, error) {
		if id == 2 {
			return dc, "arch-db", nil
		}
		return sc, "prod-db01", nil
	}}
	spec := fakeSpec()
	spec.Dest.InstanceID = 2
	if env, err = op.Open(t.Context(), spec); err == nil || env != nil || !strings.Contains(err.Error(), "归档库表所在实例") {
		t.Fatalf("目标端 sql_log_bin=0 也要拒绝：%v", err)
	}
}

func TestOpenerErrors(t *testing.T) {
	// 连不上：错误里不能出现密码
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().(*net.TCPAddr)
	ln.Close()
	op := engine.Opener{Conn: func(ctx context.Context, id uint) (eng.Conn, string, error) {
		return eng.Conn{Host: "127.0.0.1", Port: addr.Port, User: "arch", Password: "TOP-SECRET-pw"}, "x", nil
	}}
	_, err := op.Open(t.Context(), fakeSpec())
	if err == nil || strings.Contains(err.Error(), "TOP-SECRET-pw") {
		t.Fatalf("连接失败应报错且不泄漏密码：%v", err)
	}
	// 解析器报错原样返回
	op = engine.Opener{Conn: func(ctx context.Context, id uint) (eng.Conn, string, error) {
		return eng.Conn{}, "", errors.New("实例已停用")
	}}
	if _, err = op.Open(t.Context(), fakeSpec()); err == nil || err.Error() != "实例已停用" {
		t.Errorf("%v", err)
	}
	if _, err = (engine.Opener{}).Open(t.Context(), fakeSpec()); err == nil {
		t.Errorf("缺 Conn")
	}
	// 文件备份：缺 Store 解析器 / 密钥长度不对
	f := newFakeMy(1)
	op, _ = openerFor(f, t)
	spec := fakeSpec()
	spec.Dest = nil
	spec.File = &engine.FileDest{StoreID: 1, Format: "csv", PathTemplate: "a", Encrypt: true}
	if _, err = op.Open(t.Context(), spec); err == nil {
		t.Errorf("缺 Store 解析器")
	}
	op.Store = func(ctx context.Context, id uint) (engine.StoreConfig, error) {
		return engine.StoreConfig{Type: "local", Path: t.TempDir()}, nil
	}
	if _, err = op.Open(t.Context(), spec); err == nil {
		t.Errorf("缺 FileKey")
	}
	op.FileKey = func() ([]byte, error) { return []byte("short"), nil }
	if _, err = op.Open(t.Context(), spec); err == nil || !strings.Contains(err.Error(), "32") {
		t.Errorf("密钥长度：%v", err)
	}
	op.FileKey = func() ([]byte, error) { return make([]byte, 32), nil }
	env, err := op.Open(t.Context(), spec)
	if err != nil || env.Files == nil || len(env.FileKey) != 32 {
		t.Fatalf("%v", err)
	}
	env.Close()
	// Env.Close 可重复调用，nil 安全
	if err := env.Close(); err != nil {
		t.Errorf("重复关闭：%v", err)
	}
	var nilEnv *engine.Env
	if nilEnv.Close() != nil {
		t.Errorf("nil Env.Close")
	}
}

func TestEndToEndOverMySQLProtocol(t *testing.T) {
	f := newFakeMy(250)
	op, _ := openerFor(f, t)
	spec := fakeSpec()
	env, err := op.Open(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	// 试运行（只读）
	rep, err := newEng().Check(t.Context(), spec, fakePolicy(), env)
	if err != nil {
		t.Fatal(err)
	}
	if rep.HasError() {
		t.Fatalf("试运行：%+v", rep.Items)
	}
	if rep.EstRows != 250 || len(rep.Sample) != 10 || rep.Fingerprint == "" || len(rep.PKCols) != 1 {
		t.Errorf("试运行报告：%+v", rep)
	}
	if f.rowCount("shop", "orders") != 250 || f.table("shop_arch", "orders_bak") != nil {
		t.Fatalf("试运行不应改任何数据，也不应建目标表")
	}
	spec.Fingerprint = rep.Fingerprint

	// 正式运行
	res, err := newEng().Run(t.Context(), spec, fakePolicy(), env, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != engine.ResultSuccess || res.Scanned != 250 || res.Backed != 250 || res.Deleted != 250 || res.Batches != 3 || res.DestTable != "orders_bak" {
		t.Fatalf("%+v", res)
	}
	if f.rowCount("shop", "orders") != 0 || f.rowCount("shop_arch", "orders_bak") != 250 {
		t.Fatalf("源 %d 目标 %d", f.rowCount("shop", "orders"), f.rowCount("shop_arch", "orders_bak"))
	}
	// 建目标表语句：换表名、去掉 AUTO_INCREMENT 起点
	var ddl string
	for _, q := range f.queries {
		if strings.HasPrefix(q, "CREATE TABLE IF NOT EXISTS") {
			ddl = q
		}
	}
	if !strings.HasPrefix(ddl, "CREATE TABLE IF NOT EXISTS `shop_arch`.`orders_bak` (") || strings.Contains(ddl, "AUTO_INCREMENT=501") {
		t.Errorf("DDL：%s", ddl)
	}
	// 读 / 删的 SQL：带归档条件，读用 LIMIT 参数，删重复条件
	var sawRead, sawDelete, sawInsert bool
	for _, q := range f.queries {
		switch {
		case strings.HasPrefix(q, "SELECT * FROM `shop`.`orders`"):
			sawRead = true
			if !strings.HasSuffix(q, "(`created_at` < NOW() - INTERVAL 90 DAY) LIMIT ?") { // 归档并删除：不排序、没有断点
				t.Errorf("读取 SQL：%s", q)
			}
		case strings.HasPrefix(q, "DELETE FROM `shop`.`orders`"):
			sawDelete = true
			if !strings.Contains(q, "WHERE `id` IN (?") || !strings.HasSuffix(q, "AND (`created_at` < NOW() - INTERVAL 90 DAY)") {
				t.Errorf("删除 SQL 必须重复归档条件：%s", q)
			}
		case strings.HasPrefix(q, "INSERT INTO `shop_arch`.`orders_bak`"):
			sawInsert = true
		}
	}
	if !sawRead || !sawDelete || !sawInsert {
		t.Errorf("read=%v delete=%v insert=%v", sawRead, sawDelete, sawInsert)
	}
	// 断点参数是整数类型（不是字符串）
	for q, calls := range f.args {
		if strings.Contains(q, "AND `id` > ?") {
			for _, a := range calls {
				if _, ok := a[0].(int64); !ok {
					t.Errorf("断点参数应是整数：%T", a[0])
				}
			}
		}
	}
	if !f.saw("START TRANSACTION") || !f.saw("COMMIT") {
		t.Errorf("批的写入 / 删除应在事务里")
	}
	if !f.saw("SHOW GLOBAL STATUS LIKE 'Threads_running'") {
		t.Errorf("负载自检应查 Threads_running")
	}

	// 恢复：把备份表里的行补回源表（先让源表丢掉 id<=50 以外的……直接全量补）
	rspec := spec
	rspec.RunID = 2
	rng := engine.RestoreRange{DestInstanceID: 1, DestDB: "shop_arch", DestTable: res.DestTable, MinPK: res.MinPK, MaxPK: res.MaxPK}
	rres, err := newEng().Restore(t.Context(), rspec, rng, env, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rres.Result != engine.ResultSuccess || rres.Scanned != 250 || rres.Backed != 250 || f.rowCount("shop", "orders") != 250 {
		t.Fatalf("恢复：%+v 源表 %d", rres, f.rowCount("shop", "orders"))
	}
	if !f.saw("INSERT IGNORE INTO `shop`.`orders`") {
		t.Errorf("恢复应使用 INSERT IGNORE")
	}
	// 再恢复一次：不重复
	rres, _ = newEng().Restore(t.Context(), rspec, rng, env, nil, nil)
	if rres.Backed != 0 || f.rowCount("shop", "orders") != 250 {
		t.Errorf("恢复应幂等：%+v", rres)
	}
}

func TestEndToEndResumeAndStructure(t *testing.T) {
	f := newFakeMy(300)
	op, _ := openerFor(f, t)
	spec := fakeSpec()
	spec.Mode = engine.ModeCopy // 仅备份：靠“主键 > 断点”翻页，从断点继续
	spec.Checkpoint = []string{"200"}
	env, err := op.Open(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	res, err := newEng().Run(t.Context(), spec, fakePolicy(), env, nil, nil)
	if err != nil || res.Result != engine.ResultSuccess || res.Scanned != 100 || f.rowCount("shop", "orders") != 300 || len(res.Checkpoint) != 1 || res.Checkpoint[0] != "300" {
		t.Fatalf("%v %+v", err, res)
	}
	// 结构指纹不一致 → 不动数据
	spec.Mode = engine.ModeMove
	spec.Checkpoint = nil
	spec.Fingerprint = "stale"
	res, _ = newEng().Run(t.Context(), spec, fakePolicy(), env, nil, nil)
	if res.Result != engine.ResultStructure || f.rowCount("shop", "orders") != 300 {
		t.Fatalf("%+v", res)
	}
}

func TestEndToEndDestMismatchNeverDeletes(t *testing.T) {
	f := newFakeMy(200)
	op, _ := openerFor(f, t)
	spec := fakeSpec()
	// 目标表里已有 id=150 但内容不同
	f.tables["shop_arch.orders_bak"] = &fakeTbl{cols: f.tables["shop.orders"].cols, types: f.tables["shop.orders"].types, null: f.tables["shop.orders"].null, pk: []string{"id"},
		rows: [][]any{{int64(150), "2025-01-01 10:00:00", int64(0), "TAMPERED"}}}
	env, err := op.Open(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	res, err := newEng().Run(t.Context(), spec, fakePolicy(), env, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != engine.ResultVerifyFailed || !res.AutoPause {
		t.Fatalf("%+v", res)
	}
	if f.rowCount("shop", "orders") != 100 || f.rowCount("shop_arch", "orders_bak") != 101 {
		t.Fatalf("第 2 批（含冲突行）不能写入也不能删除：源 %d 目标 %d", f.rowCount("shop", "orders"), f.rowCount("shop_arch", "orders_bak"))
	}
}

func TestEndToEndProbeAndIncompatibleDest(t *testing.T) {
	f := newFakeMy(10)
	f.threads = "99"
	f.replica = true
	op, _ := openerFor(f, t)
	spec := fakeSpec()
	env, err := op.Open(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	n, err := env.Probe.ThreadsRunning(t.Context())
	if err != nil || n != 99 {
		t.Errorf("Threads_running=%d %v", n, err)
	}
	// 有从库但没配从库连接：延迟测不了，要明确报错（引擎据此标“未检测”），而不是当作没有从库
	if _, has, err := env.Probe.ReplicaLag(t.Context()); err == nil || !has {
		t.Errorf("应报“未检测”：has=%v err=%v", has, err)
	}
	if _, ok, _ := env.Probe.DiskFreePct(t.Context()); ok {
		t.Errorf("没有提供 Disk 时应为未检测")
	}
	f.replica = false
	if _, has, err := env.Probe.ReplicaLag(t.Context()); err != nil || has {
		t.Errorf("没有从库：has=%v err=%v", has, err)
	}
	// 目标表缺列：试运行红色，运行失败且不动源
	f.tables["shop_arch.orders_bak"] = &fakeTbl{cols: []string{"id", "created_at"}, types: []string{"bigint(20)", "datetime"}, null: []string{"NO", "NO"}, pk: []string{"id"}}
	rep, err := newEng().Check(t.Context(), spec, fakePolicy(), env)
	if err != nil {
		t.Fatal(err)
	}
	wantLevel(t, rep, "dest_table", "error", "缺少列")
	res, _ := newEng().Run(t.Context(), spec, fakePolicy(), env, nil, nil)
	if res.Result != engine.ResultFailed || f.rowCount("shop", "orders") != 10 {
		t.Fatalf("%+v", res)
	}
}

func TestDescribeAgainstFake(t *testing.T) {
	f := newFakeMy(42)
	op, _ := openerFor(f, t)
	env, err := op.Open(t.Context(), fakeSpec())
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	info, err := env.Src.Describe(t.Context(), "shop", "orders")
	if err != nil {
		t.Fatal(err)
	}
	if !info.Exists || info.Engine != "InnoDB" || info.Rows != 42 || info.DataMB != 3 || len(info.Columns) != 4 || info.Columns[0].Type != "bigint(20)" ||
		info.Columns[2].Nullable != true || strings.Join(info.PrimaryKey, ",") != "id" || strings.Join(info.Indexes["idx_created"], ",") != "created_at" ||
		info.Fingerprint != engine.Fingerprint(info.Columns, info.PrimaryKey) || !strings.HasPrefix(info.CreateSQL, "CREATE TABLE `orders`") {
		t.Fatalf("%+v", info)
	}
	if fp, err := env.Src.Fingerprint(t.Context(), "shop", "orders"); err != nil || fp != info.Fingerprint {
		t.Errorf("Fingerprint：%v %s", err, fp)
	}
	if _, err := env.Src.Fingerprint(t.Context(), "shop", "nope"); err == nil {
		t.Errorf("表不存在应报错")
	}
	missing, err := env.Src.Describe(t.Context(), "shop", "nope")
	if err != nil || missing.Exists {
		t.Errorf("不存在的表：%+v %v", missing, err)
	}
	if ok, err := env.Dst.DBExists(t.Context(), "shop_arch"); err != nil || !ok {
		t.Errorf("DBExists：%v %v", ok, err)
	}
	if ok, _ := env.Dst.DBExists(t.Context(), "nope"); ok {
		t.Errorf("DBExists(nope)")
	}
	n, capped, err := env.Src.CountWhere(t.Context(), "shop", "orders", "1=1", 10)
	if err != nil || n != 10 || !capped {
		t.Errorf("CountWhere：%d %v %v", n, capped, err)
	}
	ex, err := env.Src.Explain(t.Context(), "SELECT * FROM `shop`.`orders` WHERE (1=1) ORDER BY `id` LIMIT ?", 10)
	if err != nil || ex.UsedIndex != "idx_created" || ex.ScanRows != 1234 || ex.Type != "range" || ex.Extra != "Using where" {
		t.Errorf("Explain：%+v %v", ex, err)
	}
}

func TestExplainFallsBackWhenPreparedExplainUnsupported(t *testing.T) {
	f := newFakeMy(10)
	f.explainNoPS = true
	op, _ := openerFor(f, t)
	env, err := op.Open(t.Context(), fakeSpec())
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	ex, err := env.Src.Explain(t.Context(), "SELECT * FROM `shop`.`orders` WHERE (`status` = 'a?b' AND `x` = ?) ORDER BY `id` LIMIT ?", "it's", 10)
	if err != nil || ex.ScanRows != 777 || ex.Type != "ALL" {
		t.Fatalf("降级后的 EXPLAIN：%+v %v", ex, err)
	}
	var inlined string
	for _, q := range f.queries {
		if strings.HasPrefix(q, "EXPLAIN SELECT") {
			inlined = q
		}
	}
	if !strings.Contains(inlined, "`x` = 'it\\'s'") || !strings.HasSuffix(inlined, "LIMIT 10") || !strings.Contains(inlined, "'a?b'") {
		t.Errorf("内联后的语句不对：%s", inlined)
	}
}

func TestReplicaDetectionFailureIsNotTreatedAsNoReplica(t *testing.T) {
	f := newFakeMy(10)
	f.failOn = func(q string) error {
		if q == "SHOW REPLICAS" || q == "SHOW SLAVE HOSTS" {
			return errors.New("access denied; you need REPLICATION CLIENT")
		}
		return nil
	}
	op, _ := openerFor(f, t)
	env, err := op.Open(t.Context(), fakeSpec())
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	if _, has, err := env.Probe.ReplicaLag(t.Context()); err == nil {
		t.Errorf("无权限时不能当作没有从库：has=%v", has)
	}
}

// S8-B1：自动发现从库（processlist 的 Binlog Dump 线程 -> 按主机取平台里登记的从库），复制延迟变成可检测。
func TestOpenerDiscoversReplicasAndMeasuresLag(t *testing.T) {
	master := newFakeMy(10)
	master.dumpHosts = []string{"10.0.0.2:41234"}
	slave := newFakeMy(0)
	slave.lag = "3"
	op, _ := openerFor(master, t)
	sc := slave.conn(t)
	var asked []string
	op.SlaveByHost = func(ctx context.Context, host string) ([]eng.Conn, error) {
		asked = append(asked, host)
		return []eng.Conn{sc}, nil
	}
	env, err := op.Open(t.Context(), fakeSpec())
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	sec, has, err := env.Probe.ReplicaLag(t.Context())
	if err != nil || !has || sec != 3 {
		t.Fatalf("应测到从库延迟 3 秒：sec=%d has=%v err=%v", sec, has, err)
	}
	if len(asked) != 1 || asked[0] != "10.0.0.2" {
		t.Errorf("按主机（去掉端口）查从库：%v", asked)
	}
}

// 认不出的从库（平台里没登记）不能静默通过：报错，引擎标“未检测”。
func TestOpenerUnregisteredReplicaIsNotSilentlyPassed(t *testing.T) {
	master := newFakeMy(10)
	master.dumpHosts = []string{"10.0.0.9:5555", "[fe80::1]:6000"}
	op, _ := openerFor(master, t)
	op.SlaveByHost = func(ctx context.Context, host string) ([]eng.Conn, error) { return nil, nil }
	env, err := op.Open(t.Context(), fakeSpec())
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	_, has, err := env.Probe.ReplicaLag(t.Context())
	if err == nil || !has || !strings.Contains(err.Error(), "登记") {
		t.Fatalf("有从库但认不出：应报错且 has=true：has=%v err=%v", has, err)
	}
	// 查从库实例出错同样按“认不出”处理
	op.SlaveByHost = func(ctx context.Context, host string) ([]eng.Conn, error) { return nil, errors.New("meta down") }
	env2, err := op.Open(t.Context(), fakeSpec())
	if err != nil {
		t.Fatal(err)
	}
	defer env2.Close()
	if _, _, err := env2.Probe.ReplicaLag(t.Context()); err == nil {
		t.Fatal("取从库实例出错也不能当作没有从库")
	}
}

// 从库复制线程没跑（Seconds_Behind_Source 为 NULL）按“很大”处理；没有 Binlog Dump 线程且 SHOW REPLICAS 为空 = 没有从库。
func TestOpenerReplicaStoppedAndNoReplica(t *testing.T) {
	master := newFakeMy(10)
	master.dumpHosts = []string{"10.0.0.2:1"}
	slave := newFakeMy(0)
	slave.lag = "NULL"
	op, _ := openerFor(master, t)
	sc := slave.conn(t)
	op.SlaveByHost = func(ctx context.Context, host string) ([]eng.Conn, error) { return []eng.Conn{sc}, nil }
	env, err := op.Open(t.Context(), fakeSpec())
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	if sec, has, err := env.Probe.ReplicaLag(t.Context()); err != nil || !has || sec < 1<<20 {
		t.Errorf("复制线程没跑应视为延迟很大：%d %v %v", sec, has, err)
	}

	none := newFakeMy(10)
	op2, _ := openerFor(none, t)
	op2.SlaveByHost = func(ctx context.Context, host string) ([]eng.Conn, error) {
		t.Error("没有从库时不应查从库实例")
		return nil, nil
	}
	env2, err := op2.Open(t.Context(), fakeSpec())
	if err != nil {
		t.Fatal(err)
	}
	defer env2.Close()
	if _, has, err := env2.Probe.ReplicaLag(t.Context()); err != nil || has {
		t.Errorf("没有从库：has=%v err=%v", has, err)
	}
}

// 显式 Replicas 优先于自动发现；发现查询失败时退回 SHOW REPLICAS 判断。
func TestOpenerExplicitReplicasWinAndDiscoveryFailureFallsBack(t *testing.T) {
	master := newFakeMy(10)
	master.dumpHosts = []string{"10.0.0.2:1"}
	slave := newFakeMy(0)
	slave.lag = "1"
	sc := slave.conn(t)
	op, _ := openerFor(master, t)
	op.Replicas = func(ctx context.Context, id uint) ([]eng.Conn, error) { return []eng.Conn{sc}, nil }
	op.SlaveByHost = func(ctx context.Context, host string) ([]eng.Conn, error) {
		t.Error("显式 Replicas 存在时不应自动发现")
		return nil, nil
	}
	env, err := op.Open(t.Context(), fakeSpec())
	if err != nil {
		t.Fatal(err)
	}
	if sec, has, err := env.Probe.ReplicaLag(t.Context()); err != nil || !has || sec != 1 {
		t.Errorf("显式从库：%d %v %v", sec, has, err)
	}
	env.Close()

	m2 := newFakeMy(10)
	m2.replica = true
	m2.failOn = func(q string) error {
		if strings.Contains(q, "information_schema.processlist") {
			return errors.New("denied")
		}
		return nil
	}
	op2, _ := openerFor(m2, t)
	op2.SlaveByHost = func(ctx context.Context, host string) ([]eng.Conn, error) { return nil, nil }
	env2, err := op2.Open(t.Context(), fakeSpec())
	if err != nil {
		t.Fatalf("发现失败不应让 Open 失败：%v", err)
	}
	defer env2.Close()
	if _, has, err := env2.Probe.ReplicaLag(t.Context()); err == nil || !has {
		t.Errorf("发现失败时应退回 SHOW REPLICAS：有从库但测不了延迟：has=%v err=%v", has, err)
	}
}

// 探针齐全（Threads_running、从库延迟、磁盘剩余）时，试运行的 load 与 dest_disk 是绿色；缺一项是黄色。
func TestCheckLoadAndDestDiskGreenWhenProbesAvailable(t *testing.T) {
	master := newFakeMy(10)
	master.dumpHosts = []string{"10.0.0.2:1"}
	slave := newFakeMy(0)
	slave.lag = "1"
	sc := slave.conn(t)
	op, _ := openerFor(master, t)
	op.SlaveByHost = func(ctx context.Context, host string) ([]eng.Conn, error) { return []eng.Conn{sc}, nil }
	op.Disk = func(ctx context.Context, id uint) (float64, bool, error) { return 61.5, true, nil }
	env, err := op.Open(t.Context(), fakeSpec())
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	rep, err := newEng().Check(t.Context(), fakeSpec(), fakePolicy(), env)
	if err != nil {
		t.Fatal(err)
	}
	wantLevel(t, rep, "load", "ok", "61.5")
	wantLevel(t, rep, "dest_disk", "ok", "61.5")

	// 取不到磁盘（ok=false）：黄色“未检测”，不当作通过
	op.Disk = func(ctx context.Context, id uint) (float64, bool, error) { return 0, false, nil }
	env2, err := op.Open(t.Context(), fakeSpec())
	if err != nil {
		t.Fatal(err)
	}
	defer env2.Close()
	rep2, _ := newEng().Check(t.Context(), fakeSpec(), fakePolicy(), env2)
	wantLevel(t, rep2, "load", "warn", "未检测")
	wantLevel(t, rep2, "dest_disk", "warn", "未检测")
}
