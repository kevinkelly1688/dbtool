// @author: kevinkelly
// @license: Apache Licence
// @file: audit_test.go
// @time: 2026/10/28

package archive

import (
	"testing"

	"dbtool/internal/audit"
	"dbtool/internal/btnperm"
)

// 数据归档的审计动作要能被系统审计归到正确的模块 / 权限码 / 高危标记。
func TestAuditClassification(t *testing.T) {
	cases := map[string]struct {
		code  string
		risky bool
	}{
		"数据归档-提交申请": {"archive.apply", false}, "数据归档-保存草稿": {"archive.apply", false}, "数据归档-试运行": {"archive.dryrun", false},
		"数据归档-暂停任务": {"archive.pause", false}, "数据归档-恢复运行": {"archive.pause", false}, "数据归档-立即执行": {"archive.run", true},
		"数据归档-终止运行": {"archive.stop", true}, "数据归档-恢复数据": {"archive.restore", true}, "数据归档-停用任务": {"archive.delete", true},
		"数据归档-删除任务": {"archive.delete", true}, "数据归档-存储-新建": {"archive.store_edit", true}, "数据归档-策略修改": {"archive.policy_edit", true},
		"数据归档-审批通过": {"archive.view", false}, "数据归档-运行结束": {"archive.view", false},
	}
	for action, want := range cases {
		e := audit.Classify(action, "")
		if e.Code != want.code || e.Risky != want.risky || e.Category != audit.CatQuery || e.Module != "数据归档" {
			t.Errorf("%s: %+v want %+v", action, e, want)
		}
	}
}

func TestRegisteredButtons(t *testing.T) {
	for _, c := range []string{"archive.view", "archive.apply", "archive.dryrun", "archive.pause", "archive.run", "archive.stop", "archive.restore", "archive.delete",
		"archive.cfg_view", "archive.store_edit", "archive.policy_edit"} {
		b, ok := btnperm.Lookup(c)
		if !ok {
			t.Errorf("%s 未登记", c)
			continue
		}
		if (c == "archive.store_edit" || c == "archive.policy_edit") != b.Locked {
			t.Errorf("%s 锁定状态不对: %+v", c, b)
		}
	}
}
