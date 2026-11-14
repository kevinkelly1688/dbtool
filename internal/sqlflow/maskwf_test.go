// @author: kevinkelly
// @license: Apache Licence
// @file: maskwf_test.go
// @time: 2026/10/28

package sqlflow_test

import (
	"context"
	"strings"
	"testing"

	"dbtool/internal/model"
	"dbtool/internal/sqlflow"
)

// 工单 SQL 里命中“数据脱敏”配置的字段值：除超级用户外（含提交人、审批人）一律只看到脱敏后的值，审批期间也一样。
func TestWorkflowSQLMaskedForNonSuperusers(t *testing.T) {
	e := setup(t)
	e.repo.MaskCols = map[uint][]model.DataMaskingColumn{1: {
		{TableSchema: "shop", TableName_: "users", ColumnName: "password", Kind: "whole"},
		{TableSchema: "*", TableName_: "*", ColumnName: "phone", Kind: "keep", KeepPrefix: 3, KeepSuffix: 4},
	}}
	const sqlText = "update users set password='345345345', phone='13812345678' where userid=9182"
	id := e.submit(t, dev, sqlText)

	check := func(label string, d *sqlflow.Detail, wantPlain bool) {
		t.Helper()
		var all strings.Builder
		all.WriteString(d.Content.SQLContent)
		for _, r := range d.ReviewRows {
			all.WriteString("\n" + r.SQL + "\n" + r.ErrorMessage)
		}
		got := all.String()
		hasPlain := strings.Contains(got, "345345345") || strings.Contains(got, "13812345678")
		if hasPlain != wantPlain {
			t.Errorf("%s：明文可见 = %v，期望 %v\n%s", label, hasPlain, wantPlain, got)
		}
		if !wantPlain {
			if !strings.Contains(d.Content.SQLContent, "password=***") || !strings.Contains(d.Content.SQLContent, "phone=138***5678") || !strings.Contains(d.Content.SQLContent, "userid=9182") {
				t.Errorf("%s：只遮盖配置字段的值，其它保持原样：%s", label, d.Content.SQLContent)
			}
		}
	}
	d, err := e.svc.Get(dev, id)
	if err != nil {
		t.Fatal(err)
	}
	check("提交人", d, false)
	d, err = e.svc.Get(leader, id)
	if err != nil {
		t.Fatal(err)
	}
	check("审批人（审批期间）", d, false)
	d, err = e.svc.Get(root, id)
	if err != nil {
		t.Fatal(err)
	}
	check("超级用户", d, true)

	// 库里保存的仍是原文：执行用它
	if got := e.repo.Contents[id].SQLContent; got != sqlText {
		t.Errorf("库里的内容不能被改: %s", got)
	}
	// 复制：含已脱敏的值，普通用户不能复制；先走完流程到终态
	e.approve(t, id)
	if _, err := e.svc.Execute(context.Background(), dba, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CopySource(dev, id); err == nil || !strings.Contains(err.Error(), "超级用户") {
		t.Errorf("提交人不能复制含脱敏值的工单: %v", err)
	}
	if cs, err := e.svc.CopySource(root, id); err != nil || cs.SQL != sqlText {
		t.Errorf("超级用户可以复制原文: %+v %v", cs, err)
	}
	if d, err := e.svc.Get(dev, id); err != nil || d.CanCopy {
		t.Errorf("详情里不应提供复制: %+v %v", d != nil && d.CanCopy, err)
	}
	// 通知里的 SQL 预览与 sql 变量不带明文
	for _, m := range e.cap.msgs {
		if strings.Contains(m.Content, "345345345") || strings.Contains(m.Vars["sql"], "345345345") || strings.Contains(m.Vars["exec_log"], "345345345") {
			t.Errorf("通知里不能有明文: %s | %s", m.Title, m.Content)
		}
	}
}

// 没有配置脱敏字段 / 配置的字段不在语句里：原样显示。
func TestWorkflowSQLNotMaskedWithoutConfig(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "update users set password='345345345' where userid=1")
	d, err := e.svc.Get(dev, id)
	if err != nil || !strings.Contains(d.Content.SQLContent, "345345345") {
		t.Errorf("没有配置时原样: %v %+v", err, d.Content)
	}
}
