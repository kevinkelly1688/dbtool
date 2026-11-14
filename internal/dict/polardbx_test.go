// @author: kevinkelly
// @license: Apache Licence
// @file: polardbx_test.go
// @time: 2026/10/28

package dict

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// PolarDB-X / DRDS 的 information_schema 不一定支持 TABLES 的全部列和 COLUMNS 的排序：详情退到基础列与 SHOW FULL COLUMNS，
// 不能整个失败（以前直接报错，页面只剩 HTTP 502）。
func TestMySQLTableDetailFallsBackOnCompatibleEngines(t *testing.T) {
	q := &fakeQ{}
	q.fail("ROW_FORMAT", errors.New("ERR-CODE: [TDDL-4506][ERR_NOT_SUPPORT] column ROW_FORMAT"))
	q.on("TABLE_COMMENT, TABLE_ROWS, DATA_LENGTH, INDEX_LENGTH FROM information_schema.TABLES", row("sys_dictionary", "字典", int64(637), int64(16384), int64(0)))
	q.fail("information_schema.COLUMNS", errors.New("ERR-CODE: [TDDL-4506] order by"))
	q.rules = append(q.rules, rule{contains: "SHOW FULL COLUMNS", rows: &Rows{
		Columns: []string{"Field", "Type", "Collation", "Null", "Key", "Default", "Extra", "Privileges", "Comment"},
		Data:    [][]any{row("Id", "int(11)", nil, "NO", "PRI", nil, "auto_increment", "select", "主键")},
	}})
	q.fail("information_schema.STATISTICS", errors.New("not support"))
	q.on("SHOW CREATE TABLE", row("sys_dictionary", "CREATE TABLE `sys_dictionary` (`Id` int)"))

	det, err := mysqlDialect{full: true}.Detail(context.Background(), q, "shopbasic", KindTable, "", "sys_dictionary")
	if err != nil {
		t.Fatalf("兼容引擎上的表详情不应整体失败: %v", err)
	}
	by := map[string]Section{}
	for _, s := range det.Sections {
		by[s.Title] = s
	}
	if s, ok := by["表信息"]; !ok || s.Type != "kv" || !strings.Contains(s.Pairs[0].Value, "sys_dictionary") {
		t.Errorf("表信息应退到基础列: %+v", by["表信息"])
	}
	cols, ok := by["列信息"]
	if !ok || cols.Type != "table" || len(cols.Rows) != 1 || cols.Rows[0][0] != "Id" || cols.Rows[0][1] != "int(11)" || cols.Rows[0][2] != "否" || cols.Rows[0][4] != "PRI" || cols.Rows[0][6] != "主键" {
		t.Errorf("列信息应改用 SHOW FULL COLUMNS: %+v", cols)
	}
	if s := by["索引信息"]; s.Type != "note" || !strings.Contains(s.Text, "not support") {
		t.Errorf("索引失败只给说明: %+v", s)
	}
	if s := by["建表语句"]; s.Type != "code" || !strings.Contains(s.Text, "CREATE TABLE") {
		t.Errorf("建表语句: %+v", s)
	}
}

// 目标库报错用 422 返回（5xx 常被反向代理换成固定页面，看不到真正的原因）。
func TestEngineErrorIsNotServerError(t *testing.T) {
	err := engineErr("读取对象详情", errors.New("Error 1146: Table doesn't exist"))
	var e *Error
	if !errors.As(err, &e) || e.Status != 422 || !strings.Contains(e.Message, "Table doesn't exist") {
		t.Fatalf("%+v", err)
	}
}
