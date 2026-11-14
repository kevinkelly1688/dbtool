// @author: kevinkelly
// @license: Apache Licence
// @file: review_test.go
// @time: 2026/10/28

package review

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestJSONKeysMatchPython(t *testing.T) {
	s := &Set{Rows: []Row{{ID: 1, Stage: "CHECKED", StageStatus: "Audit completed", SQL: "select 1", AffectedRows: 3}}}
	var got []map[string]any
	if err := json.Unmarshal([]byte(s.JSON()), &got); err != nil {
		t.Fatal(err)
	}
	// 约定的全部键；少一个读取方就会出问题
	for _, k := range []string{"id", "stage", "errlevel", "stagestatus", "errormessage", "sql", "affected_rows", "sequence",
		"backup_dbname", "execute_time", "sqlsha1", "backup_time", "actual_affected_rows"} {
		if _, ok := got[0][k]; !ok {
			t.Errorf("缺少键 %q", k)
		}
	}
	if (&Set{}).JSON() != "[]" {
		t.Error("空结果应序列化为 []")
	}
}

func TestParseRowsTolerant(t *testing.T) {
	// 已存储的数据：affected_rows 可能是字符串、数字或 null
	rows, err := ParseRows(`[{"id":1,"errlevel":2,"stagestatus":"Execute failed","errormessage":"x","sql":"delete","affected_rows":"5","sequence":"'1_1_0'","backup_dbname":"b","sqlsha1":"","actual_affected_rows":""},
	                      {"id":"2","errlevel":0,"affected_rows":null},{"id":3,"affected_rows":7.0}]`)
	if err != nil || len(rows) != 3 {
		t.Fatalf("%v %v", rows, err)
	}
	if rows[0].AffectedRows != 5 || rows[0].ErrLevel != 2 || rows[0].BackupDBName != "b" || rows[1].ID != 2 || rows[1].AffectedRows != 0 || rows[2].AffectedRows != 7 {
		t.Errorf("容错解析: %+v", rows)
	}
	// 旧格式：每行是 goInception 原始列的数组
	rows, err = ParseRows(`[[1,"EXECUTED",0,"Execute Successfully","None","insert into t values(1)",1,"'1600_1_1'","bk_db","0.01","abc",""]]`)
	if err != nil || len(rows) != 1 || rows[0].Stage != "EXECUTED" || rows[0].AffectedRows != 1 || rows[0].BackupDBName != "bk_db" || rows[0].SQLSha1 != "abc" || rows[0].Sequence != "'1600_1_1'" {
		t.Errorf("旧格式: %+v %v", rows, err)
	}
	if rows, err := ParseRows(""); err != nil || rows != nil {
		t.Errorf("空串: %v %v", rows, err)
	}
	if _, err := ParseRows("not json"); err == nil || !strings.Contains(err.Error(), "解析失败") {
		t.Errorf("坏 JSON 应报错: %v", err)
	}
}

func TestCount(t *testing.T) {
	s := &Set{Rows: []Row{{ErrLevel: 0}, {ErrLevel: 1}, {ErrLevel: 2}, {ErrLevel: 2}}}
	s.Count()
	if s.WarningCount != 1 || s.ErrorCount != 2 {
		t.Errorf("计数: %+v", s)
	}
}

func TestRowFromColumnsShortRow(t *testing.T) {
	r := RowFromColumns([]any{int64(1), "CHECKED", int64(0)})
	if r.ID != 1 || r.Stage != "CHECKED" || r.SQL != "" || r.BackupTime != "" {
		t.Errorf("短行应补空: %+v", r)
	}
}
