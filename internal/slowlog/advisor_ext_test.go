// @author: kevinkelly
// @license: Apache Licence
// @file: advisor_ext_test.go
// @time: 2026/10/28

package slowlog

import (
	"strings"
	"testing"

	"dbtool/internal/sqlx"
)

func TestAdvisorLockWaitRule(t *testing.T) {
	sql := "update orders set a = 1 where id = 7"
	a := advise(t, AdviseRequest{SQL: sql, QueryTime: 4, LockTime: 3.2, RowsExamined: 1, RowsSent: 1})
	if !strings.Contains(codes(a), "lock_wait") {
		t.Fatalf("锁等待占大头应提示: %s", codes(a))
	}
	for _, it := range a.Items {
		if it.Code == "lock_wait" && (it.Level != "warn" || !strings.Contains(it.Title, "3.20") || !strings.Contains(it.Title, "80%")) {
			t.Fatalf("锁等待提示: %+v", it)
		}
	}
	for name, r := range map[string]AdviseRequest{
		"锁等待占比小": {SQL: sql, QueryTime: 4, LockTime: 0.5},
		"没给锁等待":  {SQL: sql, QueryTime: 4},
		"耗时为0":   {SQL: sql, QueryTime: 0, LockTime: 1},
		"锁等待太短":  {SQL: sql, QueryTime: 0.06, LockTime: 0.04},
	} {
		if strings.Contains(codes(advise(t, r)), "lock_wait") {
			t.Errorf("%s 不应提示锁等待", name)
		}
	}
}

func TestJoinConditions(t *testing.T) {
	cases := map[string][]string{
		"select 1 from a join b on a.uid = b.uid where a.x = 1":                             {"a.uid = b.uid"},
		"select 1 from a left join b on a.uid = b.uid and a.k = b.k join c on c.id = b.cid": {"a.uid = b.uid", "a.k = b.k", "c.id = b.cid"},
		"select 1 from a join b on `a`.`uid` = `b`.`uid`":                                   {"a.uid = b.uid"},
		"select 1 from a join b on a.uid = b.uid and a.st = 1 and b.flag = 'y'":             {"a.uid = b.uid"},
		"select 1 from a join b on a.uid = lower(b.uid)":                                    nil, // 函数调用不当作列
		"select 1 from a where a.uid = 1":                                                   nil,
		"insert into t(id) values (1) on duplicate key update id = id + 1":                  nil,
		"select 1 from a join b on a.uid = b.uid where a.x = b.y":                           {"a.uid = b.uid"}, // WHERE 里的不算连接条件
	}
	for sql, want := range cases {
		got := joinConditions(sqlx.Significant(sqlx.Tokenize(sql)))
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s\n 期望 %v，实际 %v", sql, want, got)
		}
	}
	a := advise(t, AdviseRequest{SQL: "select a.id from a join b on a.uid = b.uid where a.x = 1"})
	found := false
	for _, it := range a.Items {
		if it.Code == "join_index" {
			found = strings.Contains(it.Detail, "a.uid = b.uid") && it.Level == "info"
		}
	}
	if !found {
		t.Fatalf("应有连接列索引提示: %s", codes(a))
	}
}

func TestAdviceScoreAndDDL(t *testing.T) {
	// 没有任何问题：满分，没有 DDL
	a := advise(t, AdviseRequest{SQL: "select id from orders where id = 1 limit 1"})
	if a.Score != 100 {
		t.Fatalf("说明性条目不扣分: %d %s", a.Score, codes(a))
	}
	// 严重 -25、注意 -10、提示 -3
	items := []AdviceItem{{Level: "crit", Code: "a"}, {Level: "warn", Code: "b"}, {Level: "info", Code: "c"}, {Level: "info", Code: "no_findings"}}
	if s := adviceScore(items); s != 100-25-10-3 {
		t.Fatalf("评分: %d", s)
	}
	many := make([]AdviceItem, 10)
	for i := range many {
		many[i] = AdviceItem{Level: "crit", Code: "x"}
	}
	if s := adviceScore(many); s != 0 {
		t.Fatalf("最低 0: %d", s)
	}

	// 候选索引 DDL：等值在前、范围在后，带库名
	a = advise(t, AdviseRequest{DB: "shop", SQL: "select id from orders where status = 'a' and created_at > '2026-01-01' and user_id = 3"})
	if len(a.DDL) != 1 || a.DDL[0] != "ALTER TABLE `shop`.`orders` ADD INDEX `idx_status_user_id_created_at` (`status`, `user_id`, `created_at`);" {
		t.Fatalf("DDL: %v", a.DDL)
	}
	// 不带库名
	a = advise(t, AdviseRequest{SQL: "delete from orders where phone = '1'"})
	if len(a.DDL) != 1 || !strings.HasPrefix(a.DDL[0], "ALTER TABLE `orders` ADD INDEX") {
		t.Fatalf("DDL: %v", a.DDL)
	}
	// 多表 / 无 WHERE / 非 DML：不给
	for _, sql := range []string{"select a.id from a join b on a.id = b.id where a.x = 1", "select id from orders", "insert into orders(a) values (1)"} {
		if a = advise(t, AdviseRequest{SQL: sql}); len(a.DDL) != 0 {
			t.Errorf("%s 不应有 DDL: %v", sql, a.DDL)
		}
	}
	if got := candidateDDL("d`b", "t`x", []string{"a`b"}); !strings.Contains(got, "`d``b`.`t``x`") || !strings.Contains(got, "(`a``b`)") {
		t.Fatalf("标识符转义: %s", got)
	}
}
