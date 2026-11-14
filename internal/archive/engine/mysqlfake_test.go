// @author: kevinkelly
// @license: Apache Licence
// @file: mysqlfake_test.go
// @time: 2026/10/28

package engine_test

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"

	"dbtool/internal/archive/engine"
	eng "dbtool/internal/engine"
)

// fakeMy 是一个会说 MySQL 协议的假服务端（go-mysql 的 server 包），按引擎生成的 SQL 文本做结构化匹配，
// 在内存里模拟一小块 information_schema 和 shop.orders / shop_arch.* 表。用来端到端覆盖：
// Opener（会话参数、sql_log_bin）、SourceDB / DestDB / LoadProbe 的真实实现、参数顺序、事务、DDL 改写、二进制协议取值。
var reShowTbl = regexp.MustCompile("^SHOW (?:COLUMNS|INDEX) FROM `([^`]+)`\\.`([^`]+)`")

type fakeMy struct {
	server.EmptyHandler
	mu          sync.Mutex
	logBin      string
	queries     []string // 全部语句文本（含 SET）
	args        map[string][][]any
	tables      map[string]*fakeTbl
	dbs         map[string]bool
	replica     bool // SHOW REPLICAS 是否有行
	failOn      func(q string) error
	threads     string
	explainNoPS bool     // 预编译的 EXPLAIN 返回 1295
	dumpHosts   []string // processlist 里 Binlog Dump 线程的 host（形如 10.0.0.2:41234）
	lag         string   // 非空时本实例是从库：SHOW REPLICA STATUS 返回该 Seconds_Behind_Source（"NULL" = 复制线程没在跑）
}

type fakeTbl struct {
	cols  []string
	types []string
	null  []string
	pk    []string
	rows  [][]any
	ddl   string
}

func newFakeMy(rows int) *fakeMy {
	f := &fakeMy{logBin: "1", args: map[string][][]any{}, tables: map[string]*fakeTbl{}, dbs: map[string]bool{"shop": true, "shop_arch": true}, threads: "7"}
	t := &fakeTbl{
		cols: []string{"id", "created_at", "status", "payload"}, types: []string{"bigint(20)", "datetime", "int(11)", "varchar(64)"},
		null: []string{"NO", "NO", "YES", "YES"}, pk: []string{"id"},
		ddl: "CREATE TABLE `orders` (\n  `id` bigint(20) NOT NULL AUTO_INCREMENT,\n  `created_at` datetime NOT NULL,\n  `status` int(11) DEFAULT NULL,\n  `payload` varchar(64) DEFAULT NULL,\n  PRIMARY KEY (`id`),\n  KEY `idx_created` (`created_at`)\n) ENGINE=InnoDB AUTO_INCREMENT=501 DEFAULT CHARSET=utf8mb4",
	}
	for i := 1; i <= rows; i++ {
		t.rows = append(t.rows, []any{int64(i), "2025-01-01 10:00:00", int64(i % 5), fmt.Sprintf("p%d", i)})
	}
	f.tables["shop.orders"] = t
	return f
}

func (f *fakeMy) start(t *testing.T, user, pass string) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
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
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ = strconv.Atoi(p)
	return h, port
}

func (f *fakeMy) conn(t *testing.T) eng.Conn {
	host, port := f.start(t, "arch", "s3cr3t-pw")
	return eng.Conn{Host: host, Port: port, User: "arch", Password: "s3cr3t-pw"}
}

func (f *fakeMy) log(q string, args []any) {
	f.queries = append(f.queries, q)
	if args != nil {
		f.args[q] = append(f.args[q], args)
	}
}

func (f *fakeMy) saw(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, q := range f.queries {
		if strings.Contains(q, sub) {
			return true
		}
	}
	return false
}

func (f *fakeMy) table(db, name string) *fakeTbl {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tables[db+"."+name]
}

func (f *fakeMy) rowCount(db, name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t := f.tables[db+"."+name]; t != nil {
		return len(t.rows)
	}
	return -1
}

func textRS(names []string, rows ...[]any) (*mysql.Result, error) {
	if len(rows) == 0 {
		dummy := make([]any, len(names))
		for i := range dummy {
			dummy[i] = "x"
		}
		rs, err := mysql.BuildSimpleTextResultset(names, [][]any{dummy})
		if err != nil {
			return nil, err
		}
		rs.RowDatas, rs.Values = nil, nil
		return mysql.NewResult(rs), nil
	}
	rs, err := mysql.BuildSimpleTextResultset(names, rows)
	if err != nil {
		return nil, err
	}
	return mysql.NewResult(rs), nil
}

func binRS(names []string, rows ...[]any) (*mysql.Result, error) {
	if len(rows) == 0 {
		dummy := make([]any, len(names))
		for i := range dummy {
			dummy[i] = "x"
		}
		rs, err := mysql.BuildSimpleBinaryResultset(names, [][]any{dummy})
		if err != nil {
			return nil, err
		}
		rs.RowDatas, rs.Values = nil, nil
		return mysql.NewResult(rs), nil
	}
	rs, err := mysql.BuildSimpleBinaryResultset(names, rows)
	if err != nil {
		return nil, err
	}
	return mysql.NewResult(rs), nil
}

var (
	reShowCreate = regexp.MustCompile("^SHOW CREATE TABLE `([^`]+)`\\.`([^`]+)`$")
	reCreateTbl  = regexp.MustCompile("^CREATE TABLE IF NOT EXISTS `([^`]+)`\\.`([^`]+)` ")
	reReadSQL    = regexp.MustCompile("^SELECT \\* FROM `([^`]+)`\\.`([^`]+)` WHERE \\((.*?)\\)( AND (`[^`]+`|\\([^)]*\\)) > (\\?|\\([?, ]+\\)))? ORDER BY (.*) LIMIT \\?$")
	reDeleteSQL  = regexp.MustCompile("^DELETE FROM `([^`]+)`\\.`([^`]+)` WHERE (.*?) IN \\((.*)\\) AND \\((.*)\\)$")
	reInsertSQL  = regexp.MustCompile("^(INSERT(?: IGNORE)?) INTO `([^`]+)`\\.`([^`]+)` \\((.*?)\\) VALUES (.*)$")
	reSelPK      = regexp.MustCompile("^SELECT (.*?) FROM `([^`]+)`\\.`([^`]+)` WHERE `([^`]+)` IN \\(([?, ]+)\\)$")
	reRange      = regexp.MustCompile("^SELECT \\* FROM `([^`]+)`\\.`([^`]+)` WHERE `([^`]+)` >= \\? AND `[^`]+` <= \\?( AND `[^`]+` > \\?)? ORDER BY `[^`]+` LIMIT \\?$")
	reCountPK    = regexp.MustCompile("^SELECT COUNT\\(\\*\\) FROM `([^`]+)`\\.`([^`]+)` WHERE `([^`]+)` IN \\(([?, ]+)\\)$")
	reCountWhere = regexp.MustCompile("^SELECT COUNT\\(\\*\\) FROM \\(SELECT 1 FROM `([^`]+)`\\.`([^`]+)` WHERE \\((.*)\\) LIMIT \\?\\) AS t$")
	reSample     = regexp.MustCompile("^SELECT (.*?) FROM `([^`]+)`\\.`([^`]+)` WHERE \\((.*)\\) LIMIT \\?$")
	// 整型时间戳列的探测走文本协议（不带参数）
	reProbeSample = regexp.MustCompile("^SELECT `([^`]+)` FROM `([^`]+)`\\.`([^`]+)` WHERE `[^`]+` > 0 LIMIT (\\d+)$")
	reProbeRange  = regexp.MustCompile("^SELECT MIN\\(`([^`]+)`\\), MAX\\(`[^`]+`\\) FROM `([^`]+)`\\.`([^`]+)` WHERE `[^`]+` > 0$")
)

// positiveInts 取表里某列大于 0 的整数值（探测语句的 WHERE col > 0）。
func (t *fakeTbl) positiveInts(col string) []int64 {
	idx := -1
	for i, c := range t.cols {
		if c == col {
			idx = i
		}
	}
	var out []int64
	if idx < 0 {
		return out
	}
	for _, r := range t.rows {
		if v, err := strconv.ParseInt(s(r[idx]), 10, 64); err == nil && v > 0 {
			out = append(out, v)
		}
	}
	return out
}

func (f *fakeMy) HandleQuery(q string) (*mysql.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log(q, nil)
	if f.failOn != nil {
		if err := f.failOn(q); err != nil {
			return nil, err
		}
	}
	switch {
	case strings.HasPrefix(q, "SET "), q == "START TRANSACTION", q == "COMMIT", q == "ROLLBACK":
		return &mysql.Result{}, nil
	case strings.HasPrefix(q, "SELECT TIMEDIFF(NOW(), UTC_TIMESTAMP())"):
		return textRS([]string{"x"}, []any{"08:00:00"})
	case strings.HasPrefix(q, "SELECT @@session.sql_log_bin"):
		return textRS([]string{"@@session.sql_log_bin"}, []any{f.logBin})
	case q == "SHOW GLOBAL STATUS LIKE 'Threads_running'":
		return textRS([]string{"Variable_name", "Value"}, []any{"Threads_running", f.threads})
	case strings.HasPrefix(q, "EXPLAIN SELECT"):
		return textRS([]string{"id", "select_type", "table", "partitions", "type", "possible_keys", "key", "key_len", "ref", "rows", "filtered", "Extra"},
			[]any{"1", "SIMPLE", "orders", "x", "ALL", "x", "x", "5", "x", "777", "100.00", "Using where"})
	case q == "SHOW REPLICA STATUS", q == "SHOW SLAVE STATUS":
		if f.lag == "NULL" {
			return textRS([]string{"Seconds_Behind_Source"}, []any{nil})
		}
		if f.lag != "" {
			return textRS([]string{"Seconds_Behind_Source"}, []any{f.lag})
		}
		return textRS([]string{"Seconds_Behind_Source"})
	case strings.Contains(q, "information_schema.processlist"):
		var rows [][]any
		for _, h := range f.dumpHosts {
			rows = append(rows, []any{h})
		}
		return textRS([]string{"host"}, rows...)
	case strings.HasPrefix(q, "SHOW COLUMNS FROM "), strings.HasPrefix(q, "SHOW INDEX FROM "):
		m := reShowTbl.FindStringSubmatch(q)
		var t *fakeTbl
		if m != nil {
			t = f.tables[m[1]+"."+m[2]]
		}
		if t == nil {
			return nil, mysql.NewDefaultError(mysql.ER_NO_SUCH_TABLE, "x", "y")
		}
		if strings.HasPrefix(q, "SHOW COLUMNS") {
			var rows [][]any
			for i, c := range t.cols {
				rows = append(rows, []any{c, t.types[i], t.null[i], "", nil, ""})
			}
			return textRS([]string{"Field", "Type", "Null", "Key", "Default", "Extra"}, rows...)
		}
		var rows [][]any
		for i, c := range t.pk {
			rows = append(rows, []any{"orders", "0", "PRIMARY", fmt.Sprint(i + 1), c, nil})
		}
		return textRS([]string{"Table", "Non_unique", "Key_name", "Seq_in_index", "Column_name", "Sub_part"}, rows...)
	case q == "SHOW REPLICAS", q == "SHOW SLAVE HOSTS":
		if f.replica {
			return textRS([]string{"Server_Id", "Host"}, []any{"2", "10.0.0.2"})
		}
		return textRS([]string{"Server_Id", "Host"})
	}
	if m := reProbeSample.FindStringSubmatch(q); m != nil {
		t := f.tables[m[2]+"."+m[3]]
		if t == nil {
			return nil, mysql.NewDefaultError(mysql.ER_NO_SUCH_TABLE, m[2], m[3])
		}
		lim, _ := strconv.Atoi(m[4])
		var rows [][]any
		for _, v := range t.positiveInts(m[1]) {
			if len(rows) >= lim {
				break
			}
			rows = append(rows, []any{strconv.FormatInt(v, 10)})
		}
		return textRS([]string{m[1]}, rows...)
	}
	if m := reProbeRange.FindStringSubmatch(q); m != nil {
		t := f.tables[m[2]+"."+m[3]]
		if t == nil {
			return nil, mysql.NewDefaultError(mysql.ER_NO_SUCH_TABLE, m[2], m[3])
		}
		vs := t.positiveInts(m[1])
		if len(vs) == 0 {
			return textRS([]string{"MIN", "MAX"}, []any{nil, nil})
		}
		lo, hi := vs[0], vs[0]
		for _, v := range vs {
			lo, hi = min(lo, v), max(hi, v)
		}
		return textRS([]string{"MIN", "MAX"}, []any{strconv.FormatInt(lo, 10), strconv.FormatInt(hi, 10)})
	}
	if m := reShowCreate.FindStringSubmatch(q); m != nil {
		t := f.tables[m[1]+"."+m[2]]
		if t == nil {
			return nil, mysql.NewDefaultError(mysql.ER_NO_SUCH_TABLE, m[1], m[2])
		}
		return textRS([]string{"Table", "Create Table"}, []any{m[2], t.ddl})
	}
	if m := reCreateTbl.FindStringSubmatch(q); m != nil {
		key := m[1] + "." + m[2]
		if f.tables[key] == nil {
			src := f.tables["shop.orders"]
			f.tables[key] = &fakeTbl{cols: src.cols, types: src.types, null: src.null, pk: src.pk, ddl: q}
		}
		return &mysql.Result{}, nil
	}
	return nil, fmt.Errorf("fakeMy: 不认识的语句 %q", q)
}

func (f *fakeMy) HandleStmtPrepare(q string) (int, int, any, error) {
	n, inQ := 0, false
	for _, c := range q {
		switch {
		case c == '\'':
			inQ = !inQ
		case c == '?' && !inQ:
			n++
		}
	}
	return n, 0, q, nil
}

func (f *fakeMy) HandleStmtClose(any) error { return nil }

func s(v any) string {
	switch x := v.(type) {
	case mysql.TypedBytes:
		return string(x.Bytes)
	case []byte:
		return string(x)
	case string:
		return x
	}
	return fmt.Sprint(v)
}

func (f *fakeMy) HandleStmtExecute(ctx any, q string, args []any) (*mysql.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log(q, args)
	if f.failOn != nil {
		if err := f.failOn(q); err != nil {
			return nil, err
		}
	}
	// ---- information_schema ----
	switch {
	case strings.Contains(q, "FROM information_schema.TABLES"):
		t := f.tables[s(args[0])+"."+s(args[1])]
		if t == nil {
			return binRS([]string{"ENGINE", "TABLE_ROWS", "len", "TABLE_TYPE"})
		}
		return binRS([]string{"ENGINE", "TABLE_ROWS", "len", "TABLE_TYPE"}, []any{"InnoDB", int64(len(t.rows)), int64(3 << 20), "BASE TABLE"})
	case strings.Contains(q, "FROM information_schema.COLUMNS"):
		t := f.tables[s(args[0])+"."+s(args[1])]
		var rows [][]any
		for i, c := range t.cols {
			rows = append(rows, []any{c, t.types[i], t.null[i]})
		}
		return binRS([]string{"COLUMN_NAME", "COLUMN_TYPE", "IS_NULLABLE"}, rows...)
	case strings.Contains(q, "FROM information_schema.STATISTICS"):
		t := f.tables[s(args[0])+"."+s(args[1])]
		var rows [][]any
		for i, c := range t.pk {
			rows = append(rows, []any{"PRIMARY", int64(0), c, int64(i + 1), nil})
		}
		if len(t.pk) > 0 && t.cols[1] == "created_at" {
			rows = append(rows, []any{"idx_created", int64(1), "created_at", int64(1), nil})
		}
		return binRS([]string{"INDEX_NAME", "NON_UNIQUE", "COLUMN_NAME", "SEQ_IN_INDEX", "SUB_PART"}, rows...)
	case strings.Contains(q, "FROM information_schema.PARTITIONS"), strings.Contains(q, "FROM information_schema.TRIGGERS"):
		return binRS([]string{"n"}, []any{int64(0)})
	case strings.Contains(q, "FROM information_schema.REFERENTIAL_CONSTRAINTS"):
		return binRS([]string{"CONSTRAINT_SCHEMA", "TABLE_NAME"})
	case strings.Contains(q, "FROM information_schema.SCHEMATA"):
		n := int64(0)
		if f.dbs[s(args[0])] {
			n = 1
		}
		return binRS([]string{"n"}, []any{n})
	}
	if strings.HasPrefix(q, "EXPLAIN ") {
		if f.explainNoPS {
			return nil, &mysql.MyError{Code: 1295, Message: "This command is not supported in the prepared statement protocol yet", State: "HY000"}
		}
		return binRS([]string{"id", "select_type", "table", "partitions", "type", "possible_keys", "key", "key_len", "ref", "rows", "filtered", "Extra"},
			[]any{int64(1), "SIMPLE", "orders", "x", "range", "idx_created", "idx_created", "5", "x", int64(1234), "100.00", "Using where"})
	}
	// ---- 源表读取 ----
	if m := reRange.FindStringSubmatch(q); m != nil {
		t := f.tables[m[1]+"."+m[2]]
		if t == nil {
			return nil, mysql.NewDefaultError(mysql.ER_NO_SUCH_TABLE, m[1], m[2])
		}
		lo, _ := strconv.ParseInt(s(args[0]), 10, 64)
		hi, _ := strconv.ParseInt(s(args[1]), 10, 64)
		var after int64 = -1 << 62
		if m[4] != "" {
			after, _ = strconv.ParseInt(s(args[2]), 10, 64)
		}
		limit := int(args[len(args)-1].(int64))
		var rows [][]any
		for _, r := range t.rows {
			id, _ := strconv.ParseInt(s(r[0]), 10, 64)
			if id < lo || id > hi || id <= after {
				continue
			}
			rows = append(rows, r)
			if len(rows) >= limit {
				break
			}
		}
		return binRS(t.cols, rows...)
	}
	if m := reReadSQL.FindStringSubmatch(q); m != nil {
		t := f.tables[m[1]+"."+m[2]]
		if t == nil {
			return nil, mysql.NewDefaultError(mysql.ER_NO_SUCH_TABLE, m[1], m[2])
		}
		limit := int(args[len(args)-1].(int64))
		var after []string
		if m[4] != "" {
			for _, a := range args[:len(args)-1] {
				after = append(after, s(a))
			}
		}
		var rows [][]any
		for _, r := range t.rows {
			if after != nil && !(rowPKLess(t, after, r)) {
				continue
			}
			rows = append(rows, r)
			if len(rows) >= limit {
				break
			}
		}
		return binRS(t.cols, rows...)
	}
	if m := reDeleteSQL.FindStringSubmatch(q); m != nil {
		t := f.tables[m[1]+"."+m[2]]
		want := map[string]bool{}
		npk := len(t.pk)
		for i := 0; i+npk <= len(args); i += npk {
			var parts []string
			for _, a := range args[i : i+npk] {
				parts = append(parts, s(a))
			}
			want[strings.Join(parts, ",")] = true
		}
		var kept [][]any
		var n uint64
		for _, r := range t.rows {
			if want[pkOfRow(t, r)] {
				n++
				continue
			}
			kept = append(kept, r)
		}
		t.rows = kept
		return &mysql.Result{AffectedRows: n}, nil
	}
	if m := reInsertSQL.FindStringSubmatch(q); m != nil {
		t := f.tables[m[2]+"."+m[3]]
		if t == nil {
			return nil, mysql.NewDefaultError(mysql.ER_NO_SUCH_TABLE, m[2], m[3])
		}
		ncols := len(strings.Split(m[4], ","))
		var n uint64
		for i := 0; i+ncols <= len(args); i += ncols {
			row := make([]any, ncols)
			for j := 0; j < ncols; j++ {
				row[j] = norm(args[i+j])
			}
			dup := false
			for _, r := range t.rows {
				if pkOfRow(t, r) == pkOfRow(t, row) {
					dup = true
				}
			}
			if dup {
				if m[1] == "INSERT" {
					return nil, mysql.NewDefaultError(mysql.ER_DUP_ENTRY, pkOfRow(t, row), "PRIMARY")
				}
				continue
			}
			t.rows = append(t.rows, row)
			n++
		}
		sort.SliceStable(t.rows, func(i, j int) bool { return numLess(t.rows[i][0], t.rows[j][0]) })
		return &mysql.Result{AffectedRows: n}, nil
	}
	if m := reSelPK.FindStringSubmatch(q); m != nil && !strings.HasPrefix(q, "SELECT COUNT(*)") {
		t := f.tables[m[2]+"."+m[3]]
		want := map[string]bool{}
		for _, a := range args {
			want[s(a)] = true
		}
		var rows [][]any
		for _, r := range t.rows {
			if want[pkOfRow(t, r)] {
				rows = append(rows, r)
			}
		}
		return binRS(t.cols, rows...)
	}
	if m := reCountPK.FindStringSubmatch(q); m != nil {
		t := f.tables[m[1]+"."+m[2]]
		want := map[string]bool{}
		for _, a := range args {
			want[s(a)] = true
		}
		var n int64
		for _, r := range t.rows {
			if want[pkOfRow(t, r)] {
				n++
			}
		}
		return binRS([]string{"n"}, []any{n})
	}
	if m := reCountWhere.FindStringSubmatch(q); m != nil {
		t := f.tables[m[1]+"."+m[2]]
		n := int64(len(t.rows))
		if lim := args[len(args)-1].(int64); n > lim {
			n = lim
		}
		return binRS([]string{"n"}, []any{n})
	}
	if m := reSample.FindStringSubmatch(q); m != nil {
		t := f.tables[m[2]+"."+m[3]]
		lim := int(args[len(args)-1].(int64))
		var rows [][]any
		for i, r := range t.rows {
			if i >= lim {
				break
			}
			rows = append(rows, r)
		}
		return binRS(t.cols, rows...)
	}
	return nil, fmt.Errorf("fakeMy: 不认识的预编译语句 %q", q)
}

func norm(v any) any {
	switch x := v.(type) {
	case mysql.TypedBytes:
		return string(x.Bytes)
	case []byte:
		return string(x)
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case uint8:
		return int64(x)
	}
	return v
}

func pkOfRow(t *fakeTbl, r []any) string {
	var parts []string
	for _, p := range t.pk {
		for i, c := range t.cols {
			if c == p {
				parts = append(parts, s(r[i]))
			}
		}
	}
	return strings.Join(parts, ",")
}

func numLess(a, b any) bool {
	x, _ := strconv.ParseInt(s(a), 10, 64)
	y, _ := strconv.ParseInt(s(b), 10, 64)
	return x < y
}

// rowPKLess 报告 after < pk(row)（单列整数主键）。
func rowPKLess(t *fakeTbl, after []string, r []any) bool {
	x, _ := strconv.ParseInt(after[0], 10, 64)
	y, _ := strconv.ParseInt(s(r[0]), 10, 64)
	return x < y
}

var _ = engine.ModeMove
