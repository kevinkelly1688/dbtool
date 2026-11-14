// @author: kevinkelly
// @license: Apache Licence
// @file: sql_http_test.go
// @time: 2026/10/28

package retention

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"

	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
	"dbtool/internal/settings"
)

// ---- SQL 形态：不连库，只检查生成的语句 ----

func TestSQLIsParameterizedAndUsesOnlyKnownIdentifiers(t *testing.T) {
	ident := regexp.MustCompile("^[a-z_]+$")
	for _, tg := range allTargets() {
		if tg.Kind != KindTable {
			continue
		}
		for _, id := range []string{tg.Table, tg.IDCol, tg.TimeCol} {
			if !ident.MatchString(id) {
				t.Fatalf("%s: 标识符 %q 不合法", tg.Key, id)
			}
		}
		for _, c := range tg.Children {
			if !ident.MatchString(c.Table) || !ident.MatchString(c.FKCol) {
				t.Fatalf("%s: 子表 %+v 不合法", tg.Key, c)
			}
		}
		cnt, sel := countSQL(tg), selectIDsSQL(tg)
		if strings.Count(cnt, "?") != 1 || strings.Count(sel, "?") != 2 {
			t.Fatalf("%s: 时间与 LIMIT 必须是参数: %s | %s", tg.Key, cnt, sel)
		}
		if !strings.Contains(sel, "ORDER BY `"+tg.IDCol+"` LIMIT ?") || !strings.Contains(sel, "`"+tg.TimeCol+"` < ?") {
			t.Fatalf("%s: %s", tg.Key, sel)
		}
		if strings.Contains(strings.ToUpper(cnt+sel), "ALTER ") {
			t.Fatalf("不能有 ALTER: %s", sel)
		}
		if tg.Where != "" && strings.Contains(tg.Where, "?") {
			t.Fatalf("固定条件不能带参数占位: %s", tg.Where)
		}
	}
}

func TestArchiveRunOnlyTerminalAndKeepsUnpurgedBackups(t *testing.T) {
	tg, _ := targetByKey(TargetArchiveRun)
	for _, want := range []string{"result NOT IN ('running','paused')", "manifest IS NULL OR manifest = ''", "note LIKE '备份已过期清理%'", "last_run_id"} {
		if !strings.Contains(tg.Where, want) {
			t.Fatalf("归档运行清理条件缺少 %q: %s", want, tg.Where)
		}
	}
	if tg.TimeCol != "started_at" || len(tg.Children) != 2 {
		t.Fatalf("%+v", tg)
	}
}

func TestQueryLogKeepsFavorites(t *testing.T) {
	tg, _ := targetByKey(TargetQueryLog)
	if !strings.Contains(tg.Where, "favorite = 0") {
		t.Fatalf("收藏的查询不能被清理: %s", tg.Where)
	}
}

func TestDeleteBatchStatementsUseDryRun(t *testing.T) {
	db, err := gorm.Open(mysql.New(mysql.Config{DSN: "u:p@tcp(127.0.0.1:1)/x", SkipInitializeWithVersion: true}), &gorm.Config{
		DryRun: true, DisableAutomaticPing: true, SkipDefaultTransaction: true, NamingStrategy: schema.NamingStrategy{SingularTable: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	// IN ? 展开后必须是占位符而不是拼进去的数字。
	st := db.Exec("DELETE FROM "+q("dbt_audit_ext")+" WHERE "+q("audit_id")+" IN ?", []uint64{1, 2, 3})
	sqlText := st.Statement.SQL.String()
	if !strings.Contains(sqlText, "IN (?,?,?)") || strings.Contains(sqlText, "1,2,3") {
		t.Fatalf("id 列表必须参数化: %s", sqlText)
	}
	if q("a`b") != "`a``b`" {
		t.Fatalf("反引号转义: %s", q("a`b"))
	}
	for _, s := range []string{expiredExportsSQL, backupRunsSQL, markBackupPurgedSQL} {
		if strings.Contains(strings.ToUpper(s), "ALTER ") {
			t.Fatal("不能有 ALTER")
		}
	}
	if !strings.Contains(expiredExportsSQL, "w.is_offline_export = 1") || !strings.Contains(expiredExportsSQL, "workflow_finish") {
		t.Fatalf("只清已结束的导出工单: %s", expiredExportsSQL)
	}
	if strings.Contains(backupRunsSQL, "'running'") == false || !strings.Contains(backupRunsSQL, "'dryrun'") {
		t.Fatalf("只处理终态、非试运行的运行: %s", backupRunsSQL)
	}
}

// ---- HTTP ----

type apiResp struct {
	Data    json.RawMessage `json:"data"`
	Code    string          `json:"code"`
	Message string          `json:"message"`
}

func newHTTP(t *testing.T, f *fakeRepo) *servertest.Env {
	t.Helper()
	s, _, _ := newTestService(t, f)
	return servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){
		func(sv *server.Server, rt server.Routes) { register(sv, rt, s) },
	}})
}

func TestPreviewEndpointSuperuserOnlyAndAudited(t *testing.T) {
	f := newFake()
	f.table("dbt_query_log", 500, 1)
	f.set(settings.RetentionDaysKey("query_log"), "100")
	web := newHTTP(t, f)

	if w := web.Anonymous("GET", "/api/v1/retention/preview"); w.Code != 401 {
		t.Fatalf("未登录应 401: %d", w.Code)
	}
	bob := web.Login("bob")
	for _, p := range []string{"/api/v1/retention/preview", "/api/v1/retention/runs"} {
		if w := bob.Do("GET", p, nil); w.Code != 403 {
			t.Fatalf("普通用户 %s 应 403: %d", p, w.Code)
		}
	}
	if len(f.audits) != 0 {
		t.Fatal("被拒绝的请求不能写预览审计")
	}

	root := web.Login("root")
	w := root.Do("GET", "/api/v1/retention/preview", nil)
	if w.Code != 200 {
		t.Fatalf("超级用户应 200: %d %s", w.Code, w.Body.String())
	}
	var r apiResp
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	var p Preview
	if err := json.Unmarshal(r.Data, &p); err != nil {
		t.Fatal(err)
	}
	if p.Enabled || len(p.Items) != len(allTargets()) {
		t.Fatalf("%+v", p)
	}
	var got int64 = -1
	for _, it := range p.Items {
		if it.Key == TargetQueryLog {
			got = it.Candidates
		}
	}
	if got != 1 {
		t.Fatalf("dbt_query_log 预览应为 1: %d", got)
	}
	if len(f.calls) != 0 || len(*f.tables["dbt_query_log"]) != 2 {
		t.Fatal("预览接口不能删除数据")
	}
	if len(f.audits) != 1 || f.audits[0].Action != "数据清理预览" || f.audits[0].UserName != "root" {
		t.Fatalf("预览应写审计: %+v", f.audits)
	}

	w = root.Do("GET", "/api/v1/retention/runs", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"data":[]`) {
		t.Fatalf("无汇总时返回空数组: %d %s", w.Code, w.Body.String())
	}
}
