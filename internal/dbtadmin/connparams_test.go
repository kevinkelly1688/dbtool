// @author: kevinkelly
// @license: Apache Licence
// @file: connparams_test.go
// @time: 2026/10/28

package dbtadmin

import "testing"

func TestRedactMsg(t *testing.T) {
	if got := RedactMsg("Duplicate entry 'alice' for key 'PRIMARY'"); got != "Duplicate entry '***' for key '***'" {
		t.Errorf("RedactMsg = %q", got)
	}
}

func TestParseURL(t *testing.T) {
	p, err := ParseURL("mysql://u%40x:p%3Aw@db.example.com:3307/db_tool?tls=skip-verify")
	if err != nil {
		t.Fatal(err)
	}
	if p.Host != "db.example.com" || p.Port != "3307" || p.User != "u@x" || p.Pass != "p:w" || p.DB != "db_tool" || p.TLS != "skip-verify" {
		t.Errorf("解析结果不对：%+v", p)
	}
	for _, bad := range []string{"", "postgres://u:p@h/db", "mysql://h/db", "mysql://u:p@h/", "mysql://u:p@h/db?tls=weird", "mysql://u:p@/db"} {
		if _, err := ParseURL(bad); err == nil {
			t.Errorf("ParseURL(%q) 应当失败", bad)
		}
	}
}
