// @author: kevinkelly
// @license: Apache Licence
// @file: integration_test.go
// @time: 2026/10/28

package firewall

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	drv "github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"dbtool/internal/dbtadmin"
	"dbtool/internal/model"
)

// 真实 MySQL 集成测试：默认跳过。设置环境变量 FIREWALL_IT_DSN 才会运行，例如
//
//	FIREWALL_IT_DSN='root:pw@tcp(127.0.0.1:3306)/' go test -run TestIntegration ./internal/firewall
//
// 账号需要能建库建表（测试会创建 dba_fw_it_<时间戳> 库并在结束时删除）。绝不要指向生产实例。
// 验证：建表脚本可重复执行、规则 / 例外读取与过期过滤、命中记录窗口内合并、查询日志 ID 回填、用户权限组读取。
func TestIntegrationGormStore(t *testing.T) {
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
	name := fmt.Sprintf("dba_fw_it_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DROP DATABASE IF EXISTS " + name) })

	cfg.DBName = name
	cfg.ParseTime = true
	sqlDB, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	db, err := gorm.Open(gormmysql.New(gormmysql.Config{Conn: sqlDB}), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}

	// 建表脚本执行两次：必须可重复执行
	src, err := os.ReadFile("../../deploy/sql/240_firewall.sql")
	if err != nil {
		t.Fatal(err)
	}
	stmts, err := dbtadmin.SplitSQL(string(src), dbtadmin.SplitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(stmts) != 3 {
		t.Fatalf("脚本应有 3 条建表语句: %d", len(stmts))
	}
	for round := 0; round < 2; round++ {
		for _, s := range stmts {
			if err := db.Exec(s.Text).Error; err != nil {
				t.Fatalf("第 %d 次执行建表脚本失败: %v\n%s", round+1, err, s.Text)
			}
		}
	}
	if err := db.Exec("CREATE TABLE dbt_sql_users_groups (users_id INT NOT NULL, group_id INT NOT NULL)").Error; err != nil {
		t.Fatal(err)
	}
	db.Exec("INSERT INTO dbt_sql_users_groups (users_id, group_id) VALUES (7, 3), (7, 5), (8, 3)")

	st := NewGormStore(db)

	// 空表：没有规则
	if rules, err := st.LoadRules(); err != nil || len(rules) != 0 {
		t.Fatalf("空表应没有规则: %v %v", rules, err)
	}

	// 规则：启用的读出来，停用的不读；条件 JSON 往返后能通过校验
	good := Rule{Name: "禁止危险函数", Priority: 20, Enabled: true, Mode: ModeEnforce, Scenes: []string{SceneQuery, SceneWorkflow}, Env: "prod",
		InstanceIDs: []uint{1, 2}, GroupMode: "except", GroupIDs: []uint{3}, Match: "any",
		Conditions: []Condition{{T: CondKeyword, V: []string{"SLEEP", "BENCHMARK"}}, {T: CondScanRows, V: 1000000}}, Action: ActDeny, Message: "不允许"}
	norm, err := Normalize(good)
	if err != nil {
		t.Fatal(err)
	}
	rows := []model.FirewallRule{
		{Name: norm.Name, Priority: norm.Priority, Enabled: true, Mode: norm.Mode, Scenes: "query,workflow", Env: norm.Env, InstanceIDs: "1,2",
			GroupMode: norm.GroupMode, GroupIDs: "3", MatchMode: norm.Match, Conditions: ConditionsJSON(norm.Conditions), Action: norm.Action, Message: norm.Message},
		{Name: "已停用", Priority: 1, Enabled: false, Mode: "enforce", Scenes: "query", Env: "all", GroupMode: "all", MatchMode: "all",
			Conditions: `[{"t":"feature","v":"no_where"}]`, Action: "deny"},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	got, err := st.LoadRules()
	if err != nil || len(got) != 1 {
		t.Fatalf("只应读出启用的规则: %v %v", got, err)
	}
	r := got[0]
	if r.Name != good.Name || len(r.InstanceIDs) != 2 || r.GroupMode != "except" || len(r.GroupIDs) != 1 || len(r.Scenes) != 2 {
		t.Errorf("规则字段往返: %+v", r)
	}
	if _, err := compile(r); err != nil {
		t.Errorf("从库里读回的规则应能编译（数字是 float64、列表是 []any）: %v", err)
	}
	if _, err := Normalize(Rule{Name: "x", Scenes: []string{SceneQuery}, Conditions: got[0].Conditions, Action: ActWarn}); err != nil {
		t.Errorf("读回的条件应能再次通过校验: %v", err)
	}

	// 例外：过期的不读
	now := time.Now()
	db.Create(&[]model.FirewallException{
		{RuleID: got[0].ID, SubjectKind: SubjectUser, SubjectValue: "rd_zhao", Reason: "有效", ExpiresAt: now.Add(24 * time.Hour)},
		{RuleID: got[0].ID, SubjectKind: SubjectUser, SubjectValue: "old", Reason: "已过期", ExpiresAt: now.Add(-time.Hour)},
	})
	exs, err := st.LoadExceptions(now)
	if err != nil || len(exs) != 1 || exs[0].SubjectValue != "rd_zhao" {
		t.Errorf("过期的例外不应读出: %+v %v", exs, err)
	}

	// 命中记录：窗口内同一 (规则, 用户, 指纹, 结果) 合并，结果不同另起一行
	h := HitRow{RuleID: got[0].ID, RuleName: good.Name, Scene: SceneQuery, UserName: "rd_zhao", Fingerprint: "a1b2c3d4", Effective: ActDeny, SQLText: "select ?"}
	id1, err := st.RecordHit(h, HitMergeWindow)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := st.RecordHit(h, HitMergeWindow)
	if err != nil || id2 != id1 {
		t.Fatalf("窗口内应合并到同一行: %d %d %v", id1, id2, err)
	}
	var hit model.FirewallHit
	db.First(&hit, id1)
	if hit.RepeatCnt != 2 || hit.SQLText != "select ?" {
		t.Errorf("合并后次数应为 2: %+v", hit)
	}
	h.Effective = "observe"
	if id3, _ := st.RecordHit(h, HitMergeWindow); id3 == id1 {
		t.Error("结果不同应另起一行")
	}
	if id4, _ := st.RecordHit(h, 0); id4 == id1 {
		t.Error("窗口为 0 时不合并")
	}

	if err := st.AttachQueryLog([]uint{id1}, 4242); err != nil {
		t.Fatal(err)
	}
	db.First(&hit, id1)
	if hit.QueryLogID != 4242 {
		t.Errorf("查询日志 ID 应被回填: %+v", hit)
	}

	// 用户权限组
	ids, err := st.UserGroupIDs(7)
	if err != nil || len(ids) != 2 {
		t.Errorf("用户 7 属于 2 个权限组: %v %v", ids, err)
	}

	// 表不存在（脚本未执行）：等同没有规则，不是错误
	db.Exec("DROP TABLE dbt_firewall_exception")
	if exs, err := st.LoadExceptions(now); err != nil || len(exs) != 0 {
		t.Errorf("例外表不存在应返回空: %v %v", exs, err)
	}
}
