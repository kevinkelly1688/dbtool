// @author: kevinkelly
// @license: Apache Licence
// @file: exec_test.go
// @time: 2026/10/28

package goinception

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"

	"dbtool/internal/review"
)

// fakeServer 是一个会说 MySQL 协议的假 goInception/备份库：记录收到的每条语句，按回调返回结果集。
type fakeServer struct {
	server.EmptyHandler
	mu      sync.Mutex
	queries []string
	reply   func(q string) (*mysql.Result, error)
}

func (f *fakeServer) HandleQuery(q string) (*mysql.Result, error) {
	f.mu.Lock()
	f.queries = append(f.queries, q)
	f.mu.Unlock()
	return f.reply(q)
}

func (f *fakeServer) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queries) == 0 {
		return ""
	}
	return f.queries[len(f.queries)-1]
}

func startFake(t *testing.T, user, pass string, reply func(string) (*mysql.Result, error)) (*fakeServer, string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeServer{reply: reply}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				conn, err := server.NewConn(c, user, pass, f)
				if err != nil {
					return
				}
				for {
					if err := conn.HandleCommand(); err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return f, host, port
}

func rowsResult(cols []string, vals [][]any) (*mysql.Result, error) {
	rs, err := mysql.BuildSimpleTextResultset(cols, vals)
	if err != nil {
		return nil, err
	}
	return mysql.NewResult(rs), nil
}

var target = Target{Host: "10.0.0.5", Port: 3306, User: "ro", Password: "pw123"}

var checkCols = []string{"order_id", "stage", "error_level", "stage_status", "error_message", "sql", "affected_rows", "sequence", "backup_dbname", "execute_time", "sqlsha1", "backup_time"}

func TestCheckOverTheWire(t *testing.T) {
	f, host, port := startFake(t, "gi", "gipw", func(q string) (*mysql.Result, error) {
		return rowsResult(checkCols, [][]any{
			{1, "CHECKED", 0, "Audit completed", "None", "use `shop`", 0, "0_0_0", "None", "0", "", "0"},
			{2, "CHECKED", 1, "Audit completed", "表 orders 无主键", "alter table orders add c int", 120, "0_0_1", "None", "0", "5f4dcc", "0"},
			{3, "CHECKED", 2, "Audit completed", "语法错误", "alter tabel x", nil, "0_0_2", "None", "0", "", "0"},
		})
	})
	c := New(Config{Host: host, Port: port, User: "gi", Password: "gipw"})
	rows, err := c.Check(context.Background(), target, "shop", "alter table orders add c int;", CheckOpts{RealRowCount: true})
	if err != nil {
		t.Fatal(err)
	}
	q := f.last()
	for _, want := range []string{"--user='ro'", "--password='pw123'", "--host='10.0.0.5'", "--port=3306", "--check=1;", "--real_row_count=true;", "inception_magic_start;", "use `shop`;", "alter table orders add c int;\ninception_magic_commit;"} {
		if !strings.Contains(q, want) {
			t.Errorf("发给 goInception 的脚本缺少 %q:\n%s", want, q)
		}
	}
	if len(rows) != 3 || rows[1].ErrLevel != review.LevelWarning || rows[1].ErrorMessage != "表 orders 无主键" || rows[1].AffectedRows != 120 || rows[1].SQLSha1 != "5f4dcc" ||
		rows[2].ErrLevel != review.LevelError || rows[2].AffectedRows != 0 || rows[0].ID != 1 {
		t.Errorf("解析结果: %+v", rows)
	}
}

func TestExecuteOverTheWire(t *testing.T) {
	f, host, port := startFake(t, "gi", "gipw", func(q string) (*mysql.Result, error) {
		return rowsResult(checkCols, [][]any{
			{1, "EXECUTED", 0, "Execute Successfully", "None", "insert into t values(1)", 1, "'1600_1_0'", "192_168_0_5_3306_shop", "0.003", "abc", "0.001"},
			{2, "EXECUTED", 2, "Execute failed", "Duplicate entry", "insert into t values(1)", 0, "'1600_1_1'", "None", "0", "def", "0"},
		})
	})
	c := New(Config{Host: host, Port: port, User: "gi", Password: "gipw"})
	rows, err := c.Execute(context.Background(), target, "shop", "insert into t values(1);insert into t values(1)", ExecOpts{Backup: true})
	if err != nil {
		t.Fatal(err)
	}
	q := f.last()
	for _, want := range []string{"--execute=1;--ignore-warnings=1;--backup=1;--sleep=200;--sleep_rows=100", "insert into t values(1);insert into t values(1);\ninception_magic_commit;"} {
		if !strings.Contains(q, want) {
			t.Errorf("执行脚本缺少 %q:\n%s", want, q)
		}
	}
	if len(rows) != 2 || rows[0].BackupDBName != "192_168_0_5_3306_shop" || rows[0].Sequence != "'1600_1_0'" || rows[1].ErrLevel != review.LevelError || rows[1].StageStatus != "Execute failed" {
		t.Errorf("执行结果: %+v", rows)
	}
	if _, err := c.Execute(context.Background(), target, "shop", "select 1", ExecOpts{Backup: false}); err != nil || !strings.Contains(f.last(), "--backup=0") {
		t.Errorf("不备份: %v %s", err, f.last())
	}
}

func TestConnectionFailuresAndAuth(t *testing.T) {
	_, host, port := startFake(t, "gi", "gipw", func(string) (*mysql.Result, error) { return rowsResult(checkCols, nil) })
	// 账号密码错误
	bad := New(Config{Host: host, Port: port, User: "gi", Password: "wrong"})
	if _, err := bad.Check(context.Background(), target, "d", "select 1", CheckOpts{}); err == nil {
		t.Error("密码错误应报错")
	}
	// 连不上
	dead := New(Config{Host: "127.0.0.1", Port: 1, User: "gi", Password: "x"})
	if _, err := dead.Check(context.Background(), target, "d", "select 1", CheckOpts{}); err == nil {
		t.Error("连不上应报错")
	}
}

func TestForbiddenMarkersRejectedBeforeNetwork(t *testing.T) {
	c := New(Config{Host: "127.0.0.1", Port: 1}) // 不会真的连接
	for _, s := range []string{"select 1; inception_magic_commit; inception set x=1", "INCEPTION_MAGIC_START;", "select 1 /* inception_magic_commit */"} {
		_, err := c.Execute(context.Background(), target, "d", s, ExecOpts{})
		if err == nil || !strings.Contains(err.Error(), "不允许出现") {
			t.Errorf("%q 应被拒绝: %v", s, err)
		}
	}
}

func TestOscCommands(t *testing.T) {
	f, host, port := startFake(t, "gi", "gipw", func(q string) (*mysql.Result, error) {
		return rowsResult([]string{"DBNAME", "TABLENAME", "SQLSHA1", "PERCENT", "REMAINTIME", "INFOMATION"}, [][]any{{"shop", "orders", "5f4dcc", 37, "00:02:10", "copying"}})
	})
	c := New(Config{Host: host, Port: port, User: "gi", Password: "gipw"})
	rows, err := c.Osc(context.Background(), "get", "5F4dcc")
	if err != nil || len(rows) != 1 || fmt.Sprint(rows[0]["PERCENT"]) != "37" || f.last() != "inception get osc_percent '5F4dcc';" {
		t.Errorf("get: %v %v %q", rows, err, f.last())
	}
	for _, cmd := range []string{"kill", "pause", "resume"} {
		if _, err := c.Osc(context.Background(), cmd, "abc123"); err != nil || f.last() != "inception "+cmd+" osc 'abc123';" {
			t.Errorf("%s: %v %q", cmd, err, f.last())
		}
	}
	// 命令与 sha1 都不能夹带内容
	if _, err := c.Osc(context.Background(), "get; drop", "abc"); err == nil {
		t.Error("非法命令应拒绝")
	}
	for _, sha := range []string{"abc'; inception set x=1; --", "", "zz", strings.Repeat("a", 65)} {
		if _, err := c.Osc(context.Background(), "get", sha); err == nil {
			t.Errorf("非法 sha1 %q 应拒绝", sha)
		}
	}
}

func TestBackupRollbacks(t *testing.T) {
	_, host, port := startFake(t, "bk", "bkpw", func(q string) (*mysql.Result, error) {
		switch {
		case strings.Contains(q, "Inception_backup_information") && strings.Contains(q, "'1600_1_0'"):
			return rowsResult([]string{"tablename"}, [][]any{{"orders"}})
		case strings.Contains(q, "Inception_backup_information"):
			return rowsResult([]string{"tablename"}, nil) // 没有备份记录
		case strings.Contains(q, "`bk_db`.`orders`") && strings.Contains(q, "'1600_1_0'"):
			return rowsResult([]string{"rollback_statement"}, [][]any{{"DELETE FROM `shop`.`orders` WHERE id=1;"}, {"DELETE FROM `shop`.`orders` WHERE id=2;"}})
		}
		return rowsResult([]string{"x"}, nil)
	})
	b := NewBackup(BackupConfig{Host: host, Port: port, User: "bk", Password: "bkpw"})
	rows := []review.Row{
		{ID: 1, SQL: "use shop", BackupDBName: "None"},
		{ID: 2, SQL: "insert into orders values(1),(2)", BackupDBName: "bk_db", Sequence: "'1600_1_0'"},
		{ID: 3, SQL: "update orders set a=1", BackupDBName: "bk_db", Sequence: "'1600_1_9'"}, // 备份库里没有记录
	}
	got, err := b.Rollbacks(context.Background(), rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SQL != "insert into orders values(1),(2)" || got[0].Rollback != "DELETE FROM `shop`.`orders` WHERE id=1;\nDELETE FROM `shop`.`orders` WHERE id=2;" {
		t.Errorf("回滚语句: %+v", got)
	}
	// 存储的结果被篡改：库名、序号里夹带内容，必须拒绝，不能拼进 SQL
	for _, r := range []review.Row{
		{SQL: "x", BackupDBName: "bk`; drop table x; --", Sequence: "'1_1_1'"},
		{SQL: "x", BackupDBName: "bk_db", Sequence: "'1' or '1'='1"},
	} {
		if _, err := b.Rollbacks(context.Background(), []review.Row{r}); err == nil {
			t.Errorf("不合法的备份信息应拒绝: %+v", r)
		}
	}
}
