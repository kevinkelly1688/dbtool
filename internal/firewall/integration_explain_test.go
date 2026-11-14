// @author: kevinkelly
// @license: Apache Licence
// @file: integration_explain_test.go
// @time: 2026/10/28

package firewall

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	drv "github.com/go-sql-driver/mysql"
)

// 真实 MySQL 上的 EXPLAIN 事实验证：默认跳过。设置环境变量 FIREWALL_IT_DSN 才会运行，例如
//
//	FIREWALL_IT_DSN='root:pw@tcp(127.0.0.1:3306)/' go test -run TestIntegrationExplainScanRows ./internal/firewall
//
// 账号需要能建库建表（测试会创建 dba_fw_ex_<时间戳> 库并在结束时删除）。绝不要指向生产实例。
// 验证：真实的 EXPLAIN 输出（列名、NULL、类型）能被 ScanRowsFromPlan 解析；主键等值 ≈ 1 行、全表扫描 ≈ 表行数、
// DML 可以 EXPLAIN、DDL 被 ExplainSQL 挡在前面；EstimateScan 的缓存在真实往返上生效。
func TestIntegrationExplainScanRows(t *testing.T) {
	dsn := os.Getenv("FIREWALL_IT_DSN")
	if dsn == "" {
		t.Skip("未设置 FIREWALL_IT_DSN，跳过真实 MySQL 集成测试")
	}
	cfg, err := drv.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	if err := admin.PingContext(t.Context()); err != nil {
		t.Fatalf("连不上 FIREWALL_IT_DSN 指向的实例：%v", err)
	}
	name := fmt.Sprintf("dba_fw_ex_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DROP DATABASE IF EXISTS " + name) })
	cfg.DBName = name
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	mustExec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec("CREATE TABLE t (id INT PRIMARY KEY AUTO_INCREMENT, c INT NOT NULL, pad VARCHAR(50) NOT NULL DEFAULT '')")
	var vals []string
	for i := 0; i < 2000; i++ {
		vals = append(vals, fmt.Sprintf("(%d)", i%50))
	}
	mustExec("INSERT INTO t (c) VALUES " + strings.Join(vals, ","))
	mustExec("ANALYZE TABLE t")

	plan := func(ctx context.Context, q string, _ time.Duration) ([]string, [][]any, error) {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return nil, nil, WrapPlanError(err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		var out [][]any
		for rows.Next() {
			cells := make([]sql.NullString, len(cols))
			ptrs := make([]any, len(cols))
			for i := range cells {
				ptrs[i] = &cells[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return nil, nil, err
			}
			row := make([]any, len(cols))
			for i, c := range cells {
				if c.Valid {
					row[i] = c.String
				}
			}
			out = append(out, row)
		}
		return cols, out, rows.Err()
	}

	e := New(&fakeStore{}, nil)
	ctx := context.Background()
	scan := func(stmt string) int64 {
		t.Helper()
		n, err := e.EstimateScan(ctx, 1, name, stmt, plan)
		if err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
		return n
	}
	if n := scan("select * from t where id = 5"); n < 0 || n > 2 {
		t.Errorf("主键等值应只扫描 1 行: %d", n)
	}
	if n := scan("select * from t where pad = 'x'"); n < 500 || n > 20000 {
		t.Errorf("全表扫描应接近表行数 2000: %d", n)
	}
	if n := scan("update t set pad = 'y' where c = 3"); n < 500 || n > 20000 {
		t.Errorf("UPDATE 无索引条件应扫描整表: %d", n)
	}
	if n := scan("delete from t where id = 1"); n > 2 {
		t.Errorf("按主键 DELETE 应只扫描 1 行: %d", n)
	}
	if n := scan("select a.id from t a join t b on a.c = b.c where a.id < 3"); n <= 0 {
		t.Errorf("连接的扫描行数应大于 0: %d", n)
	}
	if _, err := e.EstimateScan(ctx, 1, name, "alter table t add column d int", plan); err == nil {
		t.Error("DDL 不应下发 EXPLAIN")
	}
	if _, err := e.EstimateScan(ctx, 1, name, "select * from no_such_table", plan); err == nil {
		t.Error("表不存在应返回错误（条件按未评估处理）")
	}
	before := e.ExplainStats().Calls
	scan("select * from t where id = 5") // 同一语句：缓存
	if after := e.ExplainStats(); after.Calls != before || after.CacheHits == 0 {
		t.Errorf("同一语句应命中缓存: %+v", after)
	}
}
