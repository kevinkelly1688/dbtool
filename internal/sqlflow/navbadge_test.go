// @author: kevinkelly
// @license: Apache Licence
// @file: navbadge_test.go
// @time: 2026/10/28

package sqlflow_test

import (
	"testing"

	"dbtool/internal/audit"
)

// 侧栏“SQL 上线”角标：只数当前审批节点轮到自己的待审核工单。
func TestPendingReview(t *testing.T) {
	e := setup(t)
	id1 := e.submit(t, dev, "update orders set a=1 where id=1")
	e.submit(t, dev, "update orders set a=2 where id=2")

	count := func(a audit.Actor) int {
		t.Helper()
		n, err := e.svc.PendingReview(a, 0)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(leader); n != 2 {
		t.Errorf("第一级组长应有 2 张: %d", n)
	}
	for name, who := range map[string]audit.Actor{"dba": dba, "dev": dev, "stranger": stranger, "outsider": outsider} {
		if n := count(who); n != 0 {
			t.Errorf("%s 不应有待审批: %d", name, n)
		}
	}
	if n := count(root); n != 2 {
		t.Errorf("超级用户可审批全部: %d", n)
	}

	if _, err := e.svc.Pass(leader, id1, "ok"); err != nil {
		t.Fatal(err)
	}
	if l, d := count(leader), count(dba); l != 1 || d != 1 {
		t.Errorf("组长通过一张后 leader=%d dba=%d", l, d)
	}
	if _, err := e.svc.Pass(dba, id1, "ok"); err != nil {
		t.Fatal(err)
	}
	if d := count(dba); d != 0 {
		t.Errorf("审批通过后不再待审批: %d", d)
	}
	if n, _ := e.svc.PendingReview(leader, 1); n != 1 {
		t.Errorf("limit 只限制检查的工单数: %d", n)
	}
}
