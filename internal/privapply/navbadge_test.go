// @author: kevinkelly
// @license: Apache Licence
// @file: navbadge_test.go
// @time: 2026/10/28

package privapply_test

import (
	"context"
	"testing"

	"dbtool/internal/audit"
)

// 侧栏“权限管理”角标：只数当前审批节点轮到自己的待审批申请。
func TestPendingReview(t *testing.T) {
	s, _, _ := setup()
	mustCreate(t, s, tableReq())
	mustCreate(t, s, dbReq())

	count := func(a audit.Actor) int {
		t.Helper()
		n, err := s.PendingReview(a)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(leader); n != 2 {
		t.Errorf("第一级组长应有 2 张待审批: %d", n)
	}
	if n := count(dba); n != 0 {
		t.Errorf("DBA 还没轮到: %d", n)
	}
	if n := count(dev); n != 0 {
		t.Errorf("申请人没有审批权限: %d", n)
	}
	if n := count(outsider); n != 0 {
		t.Errorf("其他资源组的同名权限组成员不能审批: %d", n)
	}
	if n := count(root); n != 2 {
		t.Errorf("超级用户可审批全部: %d", n)
	}

	// 组长通过一张后：组长少一张，DBA 多一张
	if err := s.Review(context.Background(), leader, 1, audit.ActionPass, ""); err != nil {
		t.Fatal(err)
	}
	if l, d := count(leader), count(dba); l != 1 || d != 1 {
		t.Errorf("通过一张后 leader=%d dba=%d", l, d)
	}
	// 撤回后不再计入
	if err := s.Abort(context.Background(), dev, 2, ""); err != nil {
		t.Fatal(err)
	}
	if n := count(leader); n != 0 {
		t.Errorf("撤回后: %d", n)
	}
}
