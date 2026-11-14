// @author: kevinkelly
// @license: Apache Licence
// @file: validate_test.go
// @time: 2026/10/28

package instmgr

import (
	"strings"
	"testing"
)

func TestQuoteIdent(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"shop", "`shop`", true},
		{"a`b", "`a``b`", true},
		{"中文库", "`中文库`", true},
		{"", "", false},
		{strings.Repeat("a", 64), "`" + strings.Repeat("a", 64) + "`", true},
		{strings.Repeat("a", 65), "", false},
		{"a\x00b", "", false},
		{"a\nb", "", false},
		{"\xff\xfe", "", false},
	}
	for _, c := range cases {
		got, err := quoteIdent(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("quoteIdent(%q) = %q, %v", c.in, got, err)
		}
	}
}

func TestFmtStmtAndQuoteLit(t *testing.T) {
	got := fmtStmt("CREATE USER ?@? IDENTIFIED BY ?", "o'brien", "10.%", secret("p@ss'w\rd"))
	if got != `CREATE USER 'o\'brien'@'10.%' IDENTIFIED BY '******'` {
		t.Fatal(got)
	}
	if strings.Contains(got, "p@ss") {
		t.Fatal("密码泄露到展示语句")
	}
	if got := fmtStmt("SET GLOBAL x = ?", int64(3000)); got != "SET GLOBAL x = 3000" {
		t.Fatal(got)
	}
	if got := fmtStmt("SET GLOBAL x = ?", 1.5); got != "SET GLOBAL x = 1.5" {
		t.Fatal(got)
	}
	if got := fmtStmt("LIMIT ?, ?", 0, 20); got != "LIMIT 0, 20" {
		t.Fatal(got)
	}
	if got := fmtStmt("a = ? AND b = ?", "x"); got != "a = 'x' AND b = ?" {
		t.Fatalf("参数不够时多余的 ? 原样保留: %s", got)
	}
	if got := quoteLit("a\x00\n\r\x1a\\'\""); got != `'a\0\n\r\Z\\\'"'` {
		t.Fatal(got)
	}
	args := plainArgs([]any{secret("pw"), "x", 1})
	if args[0] != "pw" || args[1] != "x" || args[2] != 1 {
		t.Fatalf("%#v", args)
	}
}

func TestValidNewDBName(t *testing.T) {
	for _, ok := range []string{"a", "shop", "shop_db", "my-db", "a1", "Shop2024", "a" + strings.Repeat("b", 62) + "c"} {
		if err := validNewDBName(ok); err != nil {
			t.Errorf("%q 应合法: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "1a", "_a", "a_", "a-", "a b", "a;b", "a`b", "a.b", "库", "sys", "MySQL", "Information_Schema", "test", strings.Repeat("a", 65)} {
		if err := validNewDBName(bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
}

func TestPasswordAndHosts(t *testing.T) {
	for _, ok := range []string{"Passw0rd!x", "a1!aaaaa", "Zz9~Zz9~Zz9~Zz9~Zz9~Zz9~Zz9~Zz9~", "p'ss\"w0rd\\!"} {
		if err := validPassword(ok); err != nil {
			t.Errorf("%q 应合法: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "short1!", "nodigits!!", "NoSpecial11", "12345678!", "has space1!", "密码Passw0rd!", "tab\t1a!aaaa", strings.Repeat("a1!", 11)} {
		if err := validPassword(bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
	hosts, err := splitHosts(" 10.%| localhost |10.% |::1| 192.168.0.0/255.255.0.0")
	if err != nil || strings.Join(hosts, ",") != "10.%,localhost,::1,192.168.0.0/255.255.0.0" {
		t.Fatalf("%v %v", hosts, err)
	}
	for _, bad := range []string{"", "|", "a'b", "a b", "a;b", "a`b", strings.Repeat("a", 61)} {
		if _, err := splitHosts(bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
	var many []string
	for i := 0; i < 21; i++ {
		many = append(many, "h"+string(rune('a'+i)))
	}
	if _, err := splitHosts(strings.Join(many, "|")); err == nil {
		t.Error("超过 20 个主机应被拒绝")
	}
}

func TestCleanTextAndEscapes(t *testing.T) {
	if s, err := cleanText("x", "  hi  ", 5, true); err != nil || s != "hi" {
		t.Fatal(s, err)
	}
	if _, err := cleanText("x", "", 5, true); err == nil {
		t.Fatal("必填")
	}
	if s, err := cleanText("x", " ", 5, false); err != nil || s != "" {
		t.Fatal(s, err)
	}
	if _, err := cleanText("x", "abcdef", 5, true); err == nil {
		t.Fatal("过长")
	}
	if _, err := cleanText("x", "a\x00", 5, true); err == nil {
		t.Fatal("控制字符")
	}
	if _, err := cleanText("x", "中文中文中", 5, true); err != nil {
		t.Fatal("按字符计数，不是字节")
	}
	if likeEscape(`a%b_c!`) != `a!%b!_c!!` {
		t.Fatal(likeEscape(`a%b_c!`))
	}
	if escapeDBPattern(`my_db%`) != `my\_db\%` {
		t.Fatal(escapeDBPattern(`my_db%`))
	}
	if clip("中文中文", 2) != "中文…" || clip("ab", 5) != "ab" {
		t.Fatal("clip")
	}
}

func TestParseVersion(t *testing.T) {
	cases := []struct {
		raw          string
		maj, min, pa int
		maria        bool
		lock, newLk  bool
	}{
		{"8.0.36", 8, 0, 36, false, true, true},
		{"8.0.0", 8, 0, 0, false, true, false},
		{"5.7.44-log", 5, 7, 44, false, true, false},
		{"5.7.5", 5, 7, 5, false, false, false},
		{"5.6.40-log", 5, 6, 40, false, false, false},
		{"8.4.2", 8, 4, 2, false, true, true},
		{"10.6.12-MariaDB-1:10.6.12+maria~ubu2004", 10, 6, 12, true, true, false},
		{"10.3.9-MariaDB", 10, 3, 9, true, false, false},
		{"", 0, 0, 0, false, false, false},
		{"garbage", 0, 0, 0, false, false, false},
	}
	for _, c := range cases {
		v := parseVersion(c.raw)
		if v.Major != c.maj || v.Minor != c.min || v.Patch != c.pa || v.MariaDB != c.maria || v.supportsAccountLock() != c.lock || v.newLocks() != c.newLk {
			t.Errorf("%q: %+v lock=%v new=%v", c.raw, v, v.supportsAccountLock(), v.newLocks())
		}
	}
}

func TestRecordAccessors(t *testing.T) {
	r := Record{"s": "x", "n": nil, "i": int64(7), "b": []byte("12"), "f": 1.5, "u": uint64(9), "ns": " 42 "}
	if r.Str("s") != "x" || r.Str("n") != "" || r.Str("b") != "12" || r.Str("f") != "1.5" || r.Str("missing") != "" {
		t.Fatal("Str")
	}
	if r.Int("i") != 7 || r.Int("b") != 12 || r.Int("u") != 9 || r.Int("ns") != 42 || r.Int("f") != 1 || r.Int("n") != 0 {
		t.Fatal("Int")
	}
	if r.Float("f") != 1.5 || r.Float("i") != 7 || r.Float("b") != 12 {
		t.Fatal("Float")
	}
	if !r.IsNull("n") || !r.IsNull("missing") || r.IsNull("s") {
		t.Fatal("IsNull")
	}
	var nilRes *Result
	if nilRes.Records() != nil {
		t.Fatal("nil 结果")
	}
	res := &Result{Columns: []string{"a", "b"}, Rows: [][]any{{"1"}, {"2", "3"}}}
	rec := res.Records()
	if len(rec) != 2 || rec[0].Str("b") != "" || rec[1].Str("b") != "3" {
		t.Fatalf("行比列短时缺的列为空: %+v", rec)
	}
}

func TestProtectReason(t *testing.T) {
	cases := []struct {
		id       int64
		user, cm string
		want     bool
	}{
		{10, "app", "Query", true}, // 自己
		{11, "system user", "Connect", true},
		{12, "repl", "Binlog Dump", true},
		{13, "repl", "Binlog Dump GTID", true},
		{14, "event_scheduler", "Daemon", true},
		{15, "app", "Daemon", true},
		{16, "app", "Sleep", false},
		{17, "app", "Query", false},
	}
	for _, c := range cases {
		if got := protectReason(c.id, 10, c.user, c.cm) != ""; got != c.want {
			t.Errorf("%+v: %v", c, got)
		}
	}
	if protectReason(1, 0, "app", "Query") != "" {
		t.Error("取不到自己的线程号（0）时，不能把线程 1 当成自己；业务线程仍可终止")
	}
}
