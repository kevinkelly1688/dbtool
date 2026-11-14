// @author: kevinkelly
// @license: Apache Licence
// @file: store_test.go
// @time: 2026/10/28

package partman

import (
	"strings"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// dryStore 用 gorm 的 DryRun 模式只生成 SQL、不执行：没有任何网络连接（DisableAutomaticPing + SkipInitializeWithVersion）。
// 用来确认 column / interval 这两个 MySQL 关键字列被正确加了反引号，以及参数化。
func dryStore(t *testing.T) *store {
	t.Helper()
	db, err := gorm.Open(mysql.New(mysql.Config{DSN: "u:p@tcp(127.0.0.1:1)/x", SkipInitializeWithVersion: true}), &gorm.Config{
		DryRun: true, DisableAutomaticPing: true, SkipDefaultTransaction: true, NamingStrategy: schema.NamingStrategy{SingularTable: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &store{db: db}
}

func lastSQL(t *testing.T, db *gorm.DB) string {
	t.Helper()
	return db.Statement.SQL.String()
}

func TestStoreSQLQuotesReservedColumns(t *testing.T) {
	r := dryStore(t)

	st := r.db.Create(&Config{InstanceID: 1, DBName: "d", TbName: "t", Column: "c", ColType: "bigint", Period: "day", Interval: 1,
		PreCreate: 2, Timezone: "UTC", CreateTime: time.Now(), LastestTime: time.Now()})
	sql := lastSQL(t, st)
	for _, want := range []string{"INSERT INTO `dbt_partman_config`", "`column`", "`interval`", "`preCreate`", "`isenable`", "`instance_id`"} {
		if !strings.Contains(sql, want) {
			t.Errorf("INSERT 缺少 %s:\n%s", want, sql)
		}
	}

	up := r.db.Model(&Config{}).Where("id = ?", 1).Updates(map[string]any{"column": "c2", "interval": 3, "preCreate": 4})
	sql = lastSQL(t, up)
	for _, want := range []string{"UPDATE `dbt_partman_config`", "`column`=?", "`interval`=?", "`preCreate`=?"} {
		if !strings.Contains(sql, want) {
			t.Errorf("UPDATE 缺少 %s:\n%s", want, sql)
		}
	}

	var out []Config
	q := r.db.Model(&Config{}).Where("instance_id IN ?", []uint{1, 2}).
		Where("(dbname LIKE ? OR tbname LIKE ? OR `column` LIKE ? OR env LIKE ?)", "%a%", "%a%", "%a%", "%a%").Order("id").Limit(20).Find(&out)
	sql = lastSQL(t, q)
	if !strings.Contains(sql, "SELECT `id`,`cnf_id`,`instance_id`,`dbname`,`tbname`,`column`,`col_type`,`period`,`interval`,`retain`,`preCreate`,`timezone`,`isenable`,`create_time`,`lastest_time`,`env` FROM `dbt_partman_config`") &&
		!strings.Contains(sql, "SELECT * FROM `dbt_partman_config`") {
		t.Errorf("SELECT:\n%s", sql)
	}

	lg := r.db.Create(&Log{CnfID: 1, ExecResult: "[]", CreateTime: time.Now()})
	if sql = lastSQL(t, lg); !strings.Contains(sql, "INSERT INTO `dbt_partman_log` (`cnf_id`,`exec_result`,`create_time`)") {
		t.Errorf("日志 INSERT:\n%s", sql)
	}
}
