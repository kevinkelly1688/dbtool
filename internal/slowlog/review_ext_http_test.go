// @author: kevinkelly
// @license: Apache Licence
// @file: review_ext_http_test.go
// @time: 2026/10/28

package slowlog

import (
	"strings"
	"testing"
)

func TestHTTPReviewMinSecondsAllAndAdvise(t *testing.T) {
	env, repo := newEnv(t)
	grantServer(env, 2, "slowlogs.view", "slowquery.view", "slowquery.advise")
	bob := env.Login("bob")
	repo.collReview = []ReviewRow{{SQLID: "m1", SQLText: "select * from t where id = 5", ExecCount: 2, InstanceID: 1}}

	// 耗时下限传到仓库；非法值 400
	var rv ReviewPage
	bob.GetJSON("/api/v1/slowlog/review?instance_id=1&start=2026-10-01&end=2026-10-06&min_seconds=1.5", &rv)
	if repo.lastReview == nil || repo.lastReview.MinSeconds != 1.5 {
		t.Fatalf("min_seconds: %+v", repo.lastReview)
	}
	if rv.Rows[0].InstanceName != "prod-mysql-01" {
		t.Fatalf("行应带实例名: %+v", rv.Rows[0])
	}
	if code, _ := call(t, bob, "GET", "/api/v1/slowlog/review?instance_id=1&min_seconds=abc", nil); code != 400 {
		t.Fatalf("非法下限: %d", code)
	}
	if code, _ := call(t, bob, "GET", "/api/v1/slowlog/review/history?instance_id=1&min_seconds=-1", nil); code != 400 {
		t.Fatalf("明细非法下限: %d", code)
	}

	// 全部实例汇总：bob 只关联实例 1、3
	var all ReviewPage
	bob.GetJSON("/api/v1/slowlog/review?all=1&start=2026-10-01&end=2026-10-06", &all)
	if !all.All || all.Source != "all" || len(repo.lastReview.Multi) != 2 {
		t.Fatalf("汇总: %+v %+v", all, repo.lastReview)
	}
	if code, _ := call(t, bob, "GET", "/api/v1/slowlog/review?all=1&instance_id=1", nil); code != 400 {
		t.Fatalf("汇总同时指定实例: %d", code)
	}

	// 优化建议：锁等待、评分、候选 DDL 都在响应里
	var adv Advice
	bob.PostJSON("/api/v1/slowlog/advise", map[string]any{"instance_id": 1, "db_name": "shop", "sql": "select * from orders where status = 'a' and uid = 3",
		"query_time": 4.0, "lock_time": 3.0, "rows_examined": 10, "rows_sent": 10}, &adv)
	if adv.Score <= 0 || adv.Score >= 100 || len(adv.DDL) != 1 || !strings.Contains(adv.DDL[0], "`shop`.`orders`") {
		t.Fatalf("建议: %+v", adv)
	}
	found := false
	for _, it := range adv.Items {
		found = found || it.Code == "lock_wait"
	}
	if !found {
		t.Fatalf("应有锁等待提示: %+v", adv.Items)
	}
	if code, _ := call(t, bob, "POST", "/api/v1/slowlog/advise", map[string]any{"instance_id": 1, "sql": "select 1", "lock_time": -1}); code != 400 {
		t.Fatalf("负的锁等待: %d", code)
	}
}

func TestHTTPSlowQueryPageNewControls(t *testing.T) {
	env, _ := newEnv(t)
	root := env.Login("root")
	code, body := root.Page("/slowquery")
	for _, want := range []string{`id="sq-min"`, `id="sq-kpis"`, `id="sq-mopen"`, `data-can-open=`, "/static/css/slowquery.css", "趋势"} {
		if !strings.Contains(body, want) {
			t.Errorf("/slowquery 缺少 %q (%d)", want, code)
		}
	}
	if strings.Contains(body, "onclick=") || strings.Contains(body, "<script>") {
		t.Error("不能有内联脚本（CSP）")
	}
}
