// @author: kevinkelly
// @license: Apache Licence
// @file: sessions_test.go
// @time: 2026/10/28

package instmgr

import (
	"context"
	"errors"
	"strings"
	"testing"
)

var procCols = []string{"id", "user", "host", "db", "command", "time", "state", "info"}

// procEnv：平台自己的连接是线程 10；11 是复制 SQL 线程；12 是从库的 Binlog Dump；13、14 是业务会话。
func procEnv() *env {
	e := newEnv()
	rows := [][]any{
		{int64(10), "dba_ro", "10.0.0.2:40000", "", "Query", int64(0), "executing", "select * from information_schema.processlist"},
		{int64(11), "system user", "", "", "Connect", int64(5000), "Replica has read all relay log", ""},
		{int64(12), "repl", "10.0.0.9:50000", "", "Binlog Dump GTID", int64(9000), "Master has sent all binlog", ""},
		{int64(13), "app_rw", "10.20.1.8:51822", "shop", "Query", int64(1832), "Sending data", "select col_secret from orders"},
		{int64(14), "report", "10.20.2.3:40113", "shop", "Sleep", int64(412), "", ""},
	}
	e.target.on("SELECT CONNECTION_ID()", rs([]string{"id"}, []any{int64(10)}))
	e.target.onFunc("information_schema.processlist", func(sql string, args []any) (*Result, error) {
		if !strings.Contains(sql, "WHERE id IN") {
			return rs(procCols, rows...), nil
		}
		want := map[int64]bool{}
		for _, a := range args {
			want[a.(int64)] = true
		}
		var out [][]any
		for _, r := range rows {
			if want[r[0].(int64)] {
				out = append(out, r)
			}
		}
		return rs(procCols, out...), nil
	})
	return e
}

func TestProcesslistFlagsProtectedThreads(t *testing.T) {
	e := procEnv()
	res, err := e.svc.Processlist(context.Background(), root, 1, ProcFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Self != 10 || len(res.Rows) != 5 {
		t.Fatalf("%+v", res)
	}
	prot := map[int64]bool{}
	for _, r := range res.Rows {
		prot[r.ID] = r.Protected
		if r.Protected && r.Reason == "" {
			t.Errorf("受保护的线程要说明原因: %+v", r)
		}
	}
	if !prot[10] || !prot[11] || !prot[12] || prot[13] || prot[14] {
		t.Fatalf("保护标记: %+v", prot)
	}
}

func TestProcesslistFilterSQL(t *testing.T) {
	e := procEnv()
	_, err := e.svc.Processlist(context.Background(), root, 1, ProcFilter{Command: "Not Sleep", User: "app_rw", DB: "shop", MinTime: 30, Q: "50%_x"})
	if err != nil {
		t.Fatal(err)
	}
	qs := e.target.queries("information_schema.processlist")
	if len(qs) != 1 {
		t.Fatalf("%+v", qs)
	}
	q := qs[0]
	for _, want := range []string{"command <> 'Sleep'", "user = ?", "db = ?", "time >= ?", "LIKE ? ESCAPE '!'", "ORDER BY time DESC"} {
		if !strings.Contains(q.sql, want) {
			t.Errorf("SQL 缺少 %q: %s", want, q.sql)
		}
	}
	if len(q.args) != 6 || q.args[0] != "app_rw" || q.args[1] != "shop" || q.args[2] != 30 || q.args[3] != "%50!%!_x%" {
		t.Fatalf("参数应走占位符并转义 LIKE 通配符: %#v", q.args)
	}
	// 具体 command 走参数
	e2 := procEnv()
	_, _ = e2.svc.Processlist(context.Background(), root, 1, ProcFilter{Command: "Sleep"})
	if c := e2.target.queries("information_schema.processlist")[0]; !strings.Contains(c.sql, "command = ?") || c.args[0] != "Sleep" {
		t.Fatalf("%+v", c)
	}
	_, err = e2.svc.Processlist(context.Background(), root, 1, ProcFilter{Command: strings.Repeat("x", 40)})
	wantErr(t, err, 400, "command")
}

func TestSessionsVisibilityAndEngines(t *testing.T) {
	e := procEnv()
	_, err := e.svc.Processlist(context.Background(), bob, 2, ProcFilter{})
	wantErr(t, err, 403, "未关联")
	_, err = e.svc.Processlist(context.Background(), bob, 3, ProcFilter{}) // mssql
	wantErr(t, err, 400, "暂不支持")
	_, err = e.svc.Processlist(context.Background(), root, 99, ProcFilter{})
	wantErr(t, err, 404, "不存在")
	if len(e.dialer.dialed) != 0 {
		t.Fatal("无权限或不支持时不能连目标库")
	}
	// doris 支持进程列表，不支持事务与锁
	if _, err := e.svc.Processlist(context.Background(), root, 4, ProcFilter{}); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Transactions(context.Background(), root, 4, TrxFilter{})
	wantErr(t, err, 400, "暂不支持")
	_, err = e.svc.LockWaits(context.Background(), root, 4)
	wantErr(t, err, 400, "暂不支持")
}

func TestPreviewKillRevalidates(t *testing.T) {
	e := procEnv()
	plan, err := e.svc.PreviewKill(context.Background(), root, 1, []KillTarget{
		{ID: 13, User: "app_rw", Host: "10.20.1.8:51822"}, // 可以
		{ID: 10}, // 平台自己的连接
		{ID: 11}, // 复制线程
		{ID: 12}, // 从库的 Binlog Dump
		{ID: 99}, // 已结束
		{ID: 14, User: "someone_else", Host: "1.2.3.4:1"}, // 线程号被别的连接复用
		{ID: 13, User: "app_rw", Host: "10.20.1.8:51822"}, // 重复
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 6 {
		t.Fatalf("重复的线程号只算一次: %+v", plan.Items)
	}
	ok := map[int64]bool{}
	reason := map[int64]string{}
	for _, it := range plan.Items {
		ok[it.ID], reason[it.ID] = it.OK, it.Reason
	}
	if !ok[13] || ok[10] || ok[11] || ok[12] || ok[99] || ok[14] {
		t.Fatalf("%+v", ok)
	}
	if !strings.Contains(reason[10], "平台当前使用的连接") || !strings.Contains(reason[11], "系统线程") || !strings.Contains(reason[12], "复制线程") ||
		!strings.Contains(reason[99], "已经结束") || !strings.Contains(reason[14], "另一个连接") {
		t.Fatalf("原因: %+v", reason)
	}
	if len(plan.Statements) != 1 || plan.Statements[0] != "KILL 13;" {
		t.Fatalf("语句由服务端生成: %v", plan.Statements)
	}
	if len(e.target.execs()) != 0 {
		t.Fatal("预览不能执行")
	}
	_, err = e.svc.PreviewKill(context.Background(), root, 1, nil)
	wantErr(t, err, 400, "选择")
	_, err = e.svc.PreviewKill(context.Background(), root, 1, []KillTarget{{ID: -1}})
	wantErr(t, err, 400, "线程号")
	many := make([]KillTarget, 51)
	for i := range many {
		many[i].ID = int64(i + 1)
	}
	_, err = e.svc.PreviewKill(context.Background(), root, 1, many)
	wantErr(t, err, 400, "最多")
}

func TestKillExecutesOnlyAllowedAndAudits(t *testing.T) {
	e := procEnv()
	_, err := e.svc.Kill(context.Background(), root, 1, []KillTarget{{ID: 13}}, false)
	wantErr(t, err, 400, "确认")
	if len(e.dialer.dialed) != 0 {
		t.Fatal("没有确认时不应连接目标库")
	}
	res, err := e.svc.Kill(context.Background(), root, 1, []KillTarget{{ID: 13, User: "app_rw", Host: "10.20.1.8:51822"}, {ID: 10}, {ID: 12}, {ID: 14}}, true)
	if err != nil {
		t.Fatal(err)
	}
	ex := e.target.execs()
	if len(ex) != 2 || ex[0].sql != "KILL 13" || ex[1].sql != "KILL 14" {
		t.Fatalf("只应终止 13 和 14，实际 %+v", ex)
	}
	byID := map[int64]KillResult{}
	for _, r := range res {
		byID[r.ID] = r
	}
	if !byID[13].OK || !byID[14].OK || byID[10].OK || byID[12].OK {
		t.Fatalf("%+v", byID)
	}
	a := e.repo.lastAudit()
	if a.Action != "终止会话" || !strings.Contains(a.ExtraInfo, "KILL 13") || !strings.Contains(a.ExtraInfo, "app_rw@10.20.1.8:51822") {
		t.Fatalf("%+v", a)
	}
	if strings.Contains(a.ExtraInfo, "col_secret") {
		t.Fatal("审计里不应记录会话正在执行的 SQL 文本")
	}
}

func TestKillHandlesFinishedThread(t *testing.T) {
	e := procEnv()
	e.target.execErr["KILL 13"] = errors.New("Error 1094 (HY000): Unknown thread id: 13")
	res, err := e.svc.Kill(context.Background(), root, 1, []KillTarget{{ID: 13}}, true)
	if err != nil || res[0].OK || res[0].Message != "该会话已经结束" {
		t.Fatalf("%+v %v", res, err)
	}
	e.target.execErr["KILL 13"] = errors.New("Error 1095: You are not owner of thread 13")
	res, _ = e.svc.Kill(context.Background(), root, 1, []KillTarget{{ID: 13}}, true)
	if res[0].OK || !strings.Contains(res[0].Message, "1095") {
		t.Fatalf("%+v", res)
	}
}

func TestTablespaceClampsPaging(t *testing.T) {
	e := newEnv()
	e.target.on("COUNT(*) AS n FROM information_schema.tables", rs([]string{"n"}, []any{"57"}))
	e.target.on("information_schema.tables", rs([]string{"table_schema", "table_name", "engine", "total_size", "table_rows", "data_size", "index_size", "data_free", "pct_free"},
		[]any{"shop", "order_items", "InnoDB", "48210.50", "186402113", "31020.10", "16100.40", "1090.00", "2.30"}))
	res, err := e.svc.Tablespace(context.Background(), root, 1, -5, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 57 || len(res.Rows) != 1 || res.Rows[0].TotalMB != 48210.5 || res.Rows[0].Rows != 186402113 || res.Rows[0].FreePct != 2.3 {
		t.Fatalf("%+v", res)
	}
	q := e.target.queries("LIMIT ?, ?")[0]
	if q.args[0] != 0 || q.args[1] != 200 {
		t.Fatalf("分页参数应被限制: %#v", q.args)
	}
	if !strings.Contains(q.sql, "table_type = 'BASE TABLE'") || !strings.Contains(q.sql, "NOT IN ('information_schema'") {
		t.Fatalf("应只统计用户表: %s", q.sql)
	}
}

func TestTransactionsFallbackToHistory(t *testing.T) {
	e := newEnv()
	e.target.on("events_statements_history", rs([]string{"thread_id", "sql_text"},
		[]any{int64(10231), "update stock set qty=qty-1"}, []any{int64(10231), "select * from stock"}))
	e.target.on("information_schema.innodb_trx", rs([]string{"trx_id", "trx_started", "trx_state", "trx_operation_state", "thread_id", "tables_locked", "rows_locked", "rows_modified", "trx_query", "trx_secs", "user", "host", "db", "command", "thread_time"},
		[]any{"421", "2026-10-06 09:02:11", "RUNNING", "", int64(10231), int64(1), int64(42180), int64(0), "", int64(1260), "app_rw", "10.20.1.8:1", "shop", "Sleep", int64(1260)},
		[]any{"422", "2026-10-06 09:20:40", "RUNNING", "", int64(10302), int64(1), int64(3), int64(2), "update x", int64(12), "app_rw", "10.20.1.9:1", "shop", "Query", int64(0)}))
	rows, err := e.svc.Transactions(context.Background(), root, 1, TrxFilter{MinSeconds: 3, IdleOnly: true})
	if err != nil || len(rows) != 2 {
		t.Fatalf("%+v %v", rows, err)
	}
	if rows[0].Info != "update stock set qty=qty-1;\nselect * from stock" || rows[1].Info != "update x" {
		t.Fatalf("当前没有语句时用最近执行的语句补上: %q / %q", rows[0].Info, rows[1].Info)
	}
	q := e.target.queries("innodb_trx")[0]
	if !strings.Contains(q.sql, "p.command = 'Sleep'") || q.args[0] != 3 {
		t.Fatalf("%+v", q)
	}
	// performance_schema 不可用时也能返回事务
	e2 := newEnv()
	e2.target.onErr("events_statements_history", errors.New("table doesn't exist"))
	e2.target.on("information_schema.innodb_trx", rs([]string{"trx_id", "thread_id", "trx_query"}, []any{"1", int64(5), ""}))
	if rows, err := e2.svc.Transactions(context.Background(), root, 1, TrxFilter{MinSeconds: -1}); err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", rows, err)
	}
}

func TestLockWaitsPicksSQLByVersion(t *testing.T) {
	cols := []string{"wait_state", "wait_started", "wait_secs", "wait_thread", "wait_sql", "wait_object", "block_thread", "block_sql", "block_state", "block_started", "block_secs"}
	row := []any{"LOCK WAIT", "2026-10-06 09:30:00", int64(35), int64(10302), "update stock set qty=qty-1", "shop.stock (PRIMARY X)", int64(10231), "select … for update", "RUNNING", "2026-10-06 09:02:11", int64(1832)}
	for _, c := range []struct{ ver, want string }{{"8.0.36", "data_lock_waits"}, {"5.7.44-log", "innodb_lock_waits"}, {"10.6.12-MariaDB-1:10.6.12+maria~ubu2004", "innodb_lock_waits"}, {"8.0.0", "innodb_lock_waits"}} {
		e := newEnv().withVersion(c.ver)
		e.target.on("lock_waits", rs(cols, row))
		rows, err := e.svc.LockWaits(context.Background(), root, 1)
		if err != nil || len(rows) != 1 || rows[0].BlockThread != 10231 || rows[0].WaitSecs != 35 || rows[0].BlockSecs != 1832 {
			t.Fatalf("%s: %+v %v", c.ver, rows, err)
		}
		last := e.target.calls[len(e.target.calls)-1].sql
		if !strings.Contains(last, c.want) || (c.want == "data_lock_waits" && strings.Contains(last, "innodb_lock_waits")) {
			t.Errorf("%s 应使用 %s，实际:\n%s", c.ver, c.want, last)
		}
	}
	e := newEnv().withVersion("8.0.36")
	e.target.onErr("lock_waits", errors.New("Table 'performance_schema.data_lock_waits' doesn't exist"))
	_, err := e.svc.LockWaits(context.Background(), root, 1)
	wantErr(t, err, 502, "目标库返回错误")
}
