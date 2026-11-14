// @author: kevinkelly
// @license: Apache Licence
// @file: lifecycle_test.go
// @time: 2026/10/28

package audit

import (
	"errors"
	"strings"
	"testing"
	"time"

	"dbtool/internal/model"
)

type fakeLife struct {
	got  []model.AuditEntry
	err  error
	boom bool
}

func (f *fakeLife) WriteLifecycleAudit(e *model.AuditEntry) error {
	if f.boom {
		panic("boom")
	}
	if f.err != nil {
		return f.err
	}
	f.got = append(f.got, *e)
	return nil
}

func TestLifecycleBestEffort(t *testing.T) {
	p := Actor{ID: 3, Username: strings.Repeat("u", 40), Display: "张三"}
	at := time.Date(2026, 10, 7, 9, 0, 0, 0, time.Local)

	f := &fakeLife{}
	Lifecycle(f, p, ActionPrefixSQLFlow+"提交", strings.Repeat("x", 2000), at)
	if len(f.got) != 1 || f.got[0].Action != "SQL上线-提交" || len([]rune(f.got[0].UserName)) != 30 || len([]rune(f.got[0].ExtraInfo)) != 900 || !f.got[0].ActionTime.Equal(at) || f.got[0].UserID != 3 {
		t.Errorf("字段截断 / 填充: %+v", f.got)
	}
	// 不支持的仓库、写失败、panic 都不能向外抛
	Lifecycle(struct{}{}, p, "x", "y", at)
	Lifecycle(nil, p, "x", "y", at)
	Lifecycle(&fakeLife{err: errors.New("db down")}, p, "x", "y", at)
	Lifecycle(&fakeLife{boom: true}, p, "x", "y", at)
}
