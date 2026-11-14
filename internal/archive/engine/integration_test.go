// @author: kevinkelly
// @license: Apache Licence
// @file: integration_test.go
// @time: 2026/10/28

package engine_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	drv "github.com/go-sql-driver/mysql"

	"dbtool/internal/archive/engine"
	eng "dbtool/internal/engine"
)

// 真实 MySQL 集成测试：默认跳过。设置环境变量 ARCHIVE_IT_DSN 才会运行，例如
//
//	ARCHIVE_IT_DSN='root:pw@tcp(127.0.0.1:3306)/' go test -run TestIntegration ./internal/archive/engine
//
// 账号需要能建库建表（测试会创建 dba_archive_it_<时间戳> 库并在结束时删除）。绝不要指向生产实例。
func itDSN(t *testing.T) (*drv.Config, *sql.DB) {
	dsn := os.Getenv("ARCHIVE_IT_DSN")
	if dsn == "" {
		t.Skip("未设置 ARCHIVE_IT_DSN，跳过真实 MySQL 集成测试")
	}
	cfg, err := drv.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.PingContext(t.Context()); err != nil {
		t.Fatalf("连不上 ARCHIVE_IT_DSN 指向的实例：%v", err)
	}
	return cfg, db
}

func TestIntegrationMoveAndRestore(t *testing.T) {
	cfg, admin := itDSN(t)
	ctx := t.Context()
	name := fmt.Sprintf("dba_archive_it_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DROP DATABASE IF EXISTS " + name) })
	arch := name + "_arch"
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+arch); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DROP DATABASE IF EXISTS " + arch) })
	mustExec := func(q string, a ...any) {
		t.Helper()
		if _, err := admin.ExecContext(ctx, q, a...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec("CREATE TABLE " + name + ".orders (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, created_at DATETIME NOT NULL, " +
		"status INT NULL, amount DECIMAL(10,2) NULL, payload VARCHAR(64) NULL, raw BLOB NULL, KEY idx_created (created_at)) ENGINE=InnoDB")
	for i := 0; i < 25; i++ {
		mustExec("INSERT INTO "+name+".orders (created_at, status, amount, payload, raw) "+
			"SELECT NOW() - INTERVAL 200 DAY, n % 5, n / 7, CONCAT('p', n), UNHEX(HEX(n)) FROM (SELECT ? * 100 + t.n AS n FROM "+
			"(SELECT 1 n UNION SELECT 2 UNION SELECT 3 UNION SELECT 4 UNION SELECT 5 UNION SELECT 6 UNION SELECT 7 UNION SELECT 8 UNION SELECT 9 UNION SELECT 10) t) x", i)
	}
	mustExec("INSERT INTO " + name + ".orders (created_at, status) VALUES (NOW(), 1), (NOW() - INTERVAL 5 DAY, 2)") // 未过期的行不能被动

	host, portStr, _ := splitHostPort(cfg.Addr)
	port, _ := strconv.Atoi(portStr)
	conn := eng.Conn{Host: host, Port: port, User: cfg.User, Password: cfg.Passwd}
	store := t.TempDir()
	op := engine.Opener{
		Conn: func(ctx context.Context, id uint) (eng.Conn, string, error) { return conn, "it-instance", nil },
		Store: func(ctx context.Context, id uint) (engine.StoreConfig, error) {
			return engine.StoreConfig{Type: "local", Path: store}, nil
		},
		FileKey: func() ([]byte, error) {
			return []byte("0123456789abcdef0123456789abcdef"), nil
		},
		TempDir: t.TempDir(),
	}
	spec := engine.Spec{
		TaskID: 1, RunID: 1, Name: "it", InstanceID: 1, DB: name, Table: "orders", TimeColumn: "created_at", RetentionDays: 90, Mode: engine.ModeMove,
		Dest:   &engine.TableDest{InstanceID: 1, DB: arch, Template: "{table}_{yyyy}"},
		File:   &engine.FileDest{StoreID: 1, Format: "csv", Gzip: true, Encrypt: true, PathTemplate: "{inst}/{db}/{table}/{run_id}"},
		Params: engine.Params{BatchSize: 100, SleepMs: 50, MaxWaitSeconds: 30},
	}
	pol := engine.Policy{MinRetentionDays: 30}
	env, err := op.Open(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	rep, err := engine.New().Check(ctx, spec, pol, env)
	if err != nil || rep.HasError() {
		t.Fatalf("试运行：%v %+v", err, rep)
	}
	if rep.EstRows != 250 {
		t.Errorf("预估命中 %d", rep.EstRows)
	}
	spec.Fingerprint = rep.Fingerprint
	res, err := engine.New().Run(ctx, spec, pol, env, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != engine.ResultSuccess || res.Deleted != 250 || res.Backed != 250 || res.Manifest == nil || res.Manifest.Rows != 250 {
		t.Fatalf("%+v", res)
	}
	var left, archived int
	admin.QueryRow("SELECT COUNT(*) FROM " + name + ".orders").Scan(&left)
	admin.QueryRow("SELECT COUNT(*) FROM " + arch + "." + res.DestTable).Scan(&archived)
	if left != 2 || archived != 250 {
		t.Fatalf("源表剩 %d（应为 2），备份表 %d（应为 250）", left, archived)
	}
	// 恢复
	rres, err := engine.New().Restore(ctx, spec, engine.RestoreRange{DestInstanceID: 1, DestDB: arch, DestTable: res.DestTable, MinPK: res.MinPK, MaxPK: res.MaxPK}, env, nil, nil)
	if err != nil || rres.Result != engine.ResultSuccess || rres.Backed != 250 {
		t.Fatalf("恢复：%v %+v", err, rres)
	}
	admin.QueryRow("SELECT COUNT(*) FROM " + name + ".orders").Scan(&left)
	if left != 252 {
		t.Errorf("恢复后源表 %d 行", left)
	}
	// 数值精度：备份表与源表逐行一致
	var diff int
	admin.QueryRow("SELECT COUNT(*) FROM " + name + ".orders o JOIN " + arch + "." + res.DestTable + " a USING (id) WHERE NOT (o.amount <=> a.amount AND o.payload <=> a.payload AND o.raw <=> a.raw AND o.created_at <=> a.created_at)").Scan(&diff)
	if diff != 0 {
		t.Errorf("%d 行与备份不一致", diff)
	}
	_ = strings.TrimSpace
}

func splitHostPort(addr string) (string, string, error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return addr, "3306", nil
	}
	return addr[:i], addr[i+1:], nil
}
