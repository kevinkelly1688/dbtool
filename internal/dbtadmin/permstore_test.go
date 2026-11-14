// @author: kevinkelly
// @license: Apache Licence
// @file: permstore_test.go
// @time: 2026/10/28

package dbtadmin

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"
)

// dbt-admin 的连接只开一个（MaxOpenConns(1)，会话级 SET 要在整个运行期里有效）并固定在 s.conn 上：权限迁移用的 GORM 连接必须走
// 这同一个连接，否则第一条查询就永远等不到空闲连接——升级脚本卡在“权限迁移”一步（1.79 的问题）。
func TestPermMigratorUsesThePinnedConnection(t *testing.T) {
	st, fake := newFakeStore(t, func(q string, _ []driver.Value) *fakeResult {
		if strings.Contains(q, "COUNT(*)") { // TablesReady 的三条查询：都当作表已创建
			return &fakeResult{cols: []string{"n"}, rows: [][]driver.Value{{int64(3)}}}
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		m, err := st.PermMigrator(ctx)
		if err != nil {
			done <- err
			return
		}
		gm, ok := m.(gormMigrator)
		if !ok {
			done <- errors.New("PermMigrator 应返回 gormMigrator")
			return
		}
		ready, err := gm.repo.TablesReady()
		if err == nil && !ready {
			err = errors.New("TablesReady 应为真（假驱动对每个 COUNT 都返回 3）")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("卡住了：迁移用的连接没有走固定连接，在 MaxOpenConns(1) 下一直等空闲连接")
	}
	if n := len(fake.queries("information_schema.tables")); n != 3 {
		t.Errorf("TablesReady 应发 3 条查询，实际 %d", n)
	}
}

// 迁移命令整体有期限：任何一步卡住都会报错退出，不会让升级脚本无限等下去。
func TestPermMigrateHasDeadline(t *testing.T) {
	if permTimeout <= 0 || permTimeout > 10*time.Minute {
		t.Fatalf("permTimeout = %v，应是几分钟以内的正数", permTimeout)
	}
}

// 整条迁移（读 → 计划 → 事务里写）走真实的 gormMigrator + btnperm.GormRepo + 固定的单个连接，数据库用脚本化的假驱动：
// 核对 GORM 在 *sql.Conn 上能开事务、提交，写出的语句和参数对。
func TestPermMigrateEndToEndOverPinnedConnection(t *testing.T) {
	row := func(v ...driver.Value) [][]driver.Value { return [][]driver.Value{v} }
	st, fake := newFakeStore(t, func(q string, _ []driver.Value) *fakeResult {
		switch {
		case strings.Contains(q, "information_schema.tables"):
			return &fakeResult{cols: []string{"n"}, rows: row(int64(3))}
		case strings.Contains(q, "FROM dbt_perm_setting"): // 没有 mode 一行：现在是兼容模式
			return &fakeResult{cols: []string{"skey", "svalue"}}
		case strings.Contains(q, "FROM dbt_auth_group g ORDER BY g.name"):
			return &fakeResult{cols: []string{"id", "name", "members"}, rows: row(int64(3), "DBA", int64(6))}
		case strings.Contains(q, "SELECT group_id, perm_code FROM dbt_group_perm"):
			return &fakeResult{cols: []string{"group_id", "perm_code"}}
		case strings.Contains(q, "SELECT DISTINCT p.codename FROM dbt_auth_group_permissions gp"):
			return &fakeResult{cols: []string{"codename"}, rows: [][]driver.Value{{"menu_sqlworkflow"}, {"sql_review"}, {"menu_tools"}}}
		case strings.Contains(q, "SELECT up.users_id AS user_id, p.codename"):
			return &fakeResult{cols: []string{"user_id", "codename", "app_label"}, rows: row(int64(31), "query_all_instances", "sql")}
		case strings.Contains(q, "FROM dbt_sql_users WHERE is_active = 1"), strings.Contains(q, "FROM dbt_sql_users WHERE id = ?"):
			return &fakeResult{cols: []string{"id", "username", "display", "active", "superuser"}, rows: row(int64(31), "ives", "ives", true, false)}
		case strings.Contains(q, "SELECT x.uid AS user_id"):
			return &fakeResult{cols: []string{"user_id", "codename", "app_label"}, rows: row(int64(31), "query_all_instances", "sql")}
		case strings.Contains(q, "FROM dbt_group_perm gp"), strings.Contains(q, "FROM dbt_sql_users_groups ug JOIN dbt_auth_group g"):
			return &fakeResult{cols: []string{"user_id", "perm_code", "name"}}
		case strings.Contains(q, "FROM dbt_user_perm WHERE user_id"):
			return &fakeResult{cols: []string{"perm_code", "effect", "expire_at", "reason", "created_by", "created_at"}}
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type out struct {
		err   error
		adds  int
		users int
		res   string
	}
	done := make(chan out, 1)
	go func() {
		m, err := st.PermMigrator(ctx)
		if err != nil {
			done <- out{err: err}
			return
		}
		plan, err := m.Plan()
		if err != nil {
			done <- out{err: err}
			return
		}
		ga, ua := plan.Counts()
		if !plan.SetStrict || len(plan.Groups) != 1 || len(plan.Groups[0].Unmapped) != 1 || plan.Groups[0].Unmapped[0] != "menu_tools" {
			done <- out{err: errors.New("计划不对：应切到严格、DBA 组丢弃 menu_tools")}
			return
		}
		res, err := m.Apply(plan, "dbt-admin(tester)", time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC))
		if err != nil {
			done <- out{err: err}
			return
		}
		done <- out{adds: ga, users: ua, res: strings.Join(res.Rollback, "\n")}
	}()
	var o out
	select {
	case o = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("卡住了：迁移没有在固定连接上完成")
	}
	if o.err != nil {
		t.Fatal(o.err)
	}
	if o.adds < 3 || o.users != 1 {
		t.Errorf("新增数量不对：权限组 %d，用户 %d", o.adds, o.users)
	}
	if fake.begins != 1 || fake.commits != 1 || fake.rollback != 0 {
		t.Errorf("应在一个事务里提交：begin=%d commit=%d rollback=%d", fake.begins, fake.commits, fake.rollback)
	}
	for _, sub := range []string{"INSERT IGNORE INTO dbt_group_perm", "INSERT INTO dbt_user_perm", "INSERT INTO dbt_perm_setting", "dbt_audit_log"} {
		if len(fake.queries(sub)) == 0 {
			t.Errorf("没有写出 %s 的语句", sub)
		}
	}
	if q := fake.queries("INSERT IGNORE INTO dbt_group_perm"); len(q) != 1 || q[0].args[0] != int64(3) && q[0].args[0] != uint64(3) {
		t.Errorf("应只给 DBA 组（id 3）写一条批量插入：%v", q)
	}
	if q := fake.queries("INSERT INTO dbt_perm_setting"); len(q) == 1 && (q[0].args[0] != "mode" || q[0].args[1] != "strict") {
		t.Errorf("应写 mode=strict：%v", q[0].args)
	}
	if !strings.Contains(o.res, "DELETE FROM dbt_perm_setting WHERE skey = 'mode';") {
		t.Errorf("撤销脚本里缺少删除 mode：\n%s", o.res)
	}
}
