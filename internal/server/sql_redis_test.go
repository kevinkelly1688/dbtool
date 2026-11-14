// @author: kevinkelly
// @license: Apache Licence
// @file: sql_redis_test.go
// @time: 2026/10/28

package server_test

import (
	"encoding/json"
	"strings"
	"testing"

	"dbtool/internal/engine"
	"dbtool/internal/engine/redis"
	"dbtool/internal/engine/redis/redistest"
	"dbtool/internal/model"
	"dbtool/internal/redisrule"
)

// Redis 工单上线的 HTTP 层：检查 / 提交 / 审批 / 执行 / 详情页；MySQL 与 SQL Server 的页面与接口不受影响。
func TestSQLWorkflowRedisHTTP(t *testing.T) {
	e := newSQLEnv(t)
	kv := redistest.NewKV()
	srv, err := kv.Serve()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	kv.SetString(2, "cache:a", "old")
	cw := []model.InstanceTag{{TagCode: "can_write", Active: true}, {TagCode: "can_read", Active: true}}
	e.mem.Instances = append(e.mem.Instances, model.Instance{ID: 21, InstanceName: "redis-prod", DBType: "redis", Host: srv.Host(), Port: srv.Port(), Tags: cw})
	e.mem.InstRGs[21] = []uint{1}
	// 其他类型仍走原来的假引擎；redis 走真实的引擎（连进程内的假 Redis）
	e.svc.SetOpener(func(dbType string, c engine.Conn) (engine.Engine, error) {
		if dbType == "redis" {
			return redis.New(c), nil
		}
		return &hostEngine{sqlFake: e.fake, host: c.Host, failHost: e.failHost}, nil
	})
	rules := redisrule.NewMemRepo(redisrule.Rule{Name: "清理缓存", Enabled: true, Commands: []string{"DEL", "SET"}, KeyPatterns: []string{"cache:*"}, MaxKeys: 10, RecordBefore: true})
	e.svc.SetRedisRules(redisrule.SourceOf(rules))
	dev, leader := e.login(t, "dev"), e.login(t, "leader")

	// 选项：Redis 出现在上线实例里，不出现在导出实例里；库是纯数字编号
	w := dev.do("GET", "/api/v1/sqlworkflow/options", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"redis-prod"`) {
		t.Fatalf("选项: %d %s", w.Code, w.Body.String())
	}
	var opt struct {
		Data struct {
			Groups []struct {
				Instances []struct {
					Name   string `json:"name"`
					DBType string `json:"db_type"`
				} `json:"instances"`
				ExportInstances []struct{ Name string } `json:"export_instances"`
			} `json:"groups"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &opt); err != nil {
		t.Fatal(err)
	}
	foundUp, foundExp := false, false
	for _, i := range opt.Data.Groups[0].Instances {
		foundUp = foundUp || i.Name == "redis-prod" && i.DBType == "redis"
	}
	for _, i := range opt.Data.Groups[0].ExportInstances {
		foundExp = foundExp || i.Name == "redis-prod"
	}
	if !foundUp || foundExp {
		t.Errorf("Redis 实例应只出现在上线列表: up=%v export=%v", foundUp, foundExp)
	}
	if w := dev.do("GET", "/api/v1/sqlworkflow/instances/21/databases", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"0","1"`) || strings.Contains(w.Body.String(), "db0") {
		t.Errorf("库: %d %s", w.Code, w.Body.String())
	}
	// 其他类型的库列表不变
	if w := dev.do("GET", "/api/v1/sqlworkflow/instances/1/databases", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "report") {
		t.Errorf("MySQL 库: %d %s", w.Code, w.Body.String())
	}

	// 检查：逐行
	body := map[string]any{"instance_id": 21, "db_name": "2", "sql": "DEL cache:a\nDEL other:b\nFLUSHALL"}
	w = dev.do("POST", "/api/v1/sqlworkflow/check", body)
	d := dataOf(t, w)
	if w.Code != 200 || d["source"] != "redis" || d["error_count"].(float64) != 2 || !strings.Contains(d["notice"].(string), "没有事务回滚") {
		t.Fatalf("检查: %d %s", w.Code, w.Body.String())
	}
	// 提交：有被拒绝的行 400；跳过检查 400；导出 400
	sub := map[string]any{"name": "清理缓存", "instance_id": 21, "db_name": "2", "group_name": "研发组", "sql": "DEL cache:a\nDEL other:b"}
	if w := dev.do("POST", "/api/v1/sqlworkflow", sub); w.Code != 400 || !strings.Contains(w.Body.String(), "有命令被拒绝") {
		t.Errorf("有拒绝行不能提交: %d %s", w.Code, w.Body.String())
	}
	if w := dev.do("POST", "/api/v1/sqlworkflow", map[string]any{"name": "x", "instance_id": 21, "db_name": "2", "group_name": "研发组", "sql": "SET cache:a 1", "skip_check": true}); w.Code != 400 && w.Code != 403 {
		t.Errorf("跳过检查: %d", w.Code)
	}

	// 提交成功 → 审批 → 执行
	sub["sql"] = "SET cache:a new EX 60\nDEL cache:gone"
	w = dev.do("POST", "/api/v1/sqlworkflow", sub)
	dd := dataOf(t, w)
	if w.Code != 200 || dd["status"] != model.WfManReviewing {
		t.Fatalf("提交: %d %s", w.Code, w.Body.String())
	}
	id := itoa(dd["workflow_id"].(float64))
	page := dev.get("/sqlworkflow/" + id)
	for _, want := range []string{"Redis 工单：", "没有事务回滚", "Redis 命令（原文）", "无自动回滚（Redis 没有事务回滚）"} {
		if !strings.Contains(page.Body.String(), want) {
			t.Errorf("详情页缺少 %q", want)
		}
	}
	if strings.Contains(page.Body.String(), "执行前备份") {
		t.Error("Redis 工单不显示“执行前备份”")
	}
	if w := leader.do("POST", "/api/v1/sqlworkflow/"+id+"/pass", map[string]any{"remark": "ok"}); w.Code != 200 {
		t.Fatalf("审批: %d %s", w.Code, w.Body.String())
	}
	if rp := dev.get("/sqlworkflow/" + id); !strings.Contains(rp.Body.String(), "原生执行（直连 Redis") {
		t.Error("执行区只有原生执行")
	}
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/execute", map[string]any{"mode": "auto"}); w.Code != 200 {
		t.Fatalf("执行: %d %s", w.Code, w.Body.String())
	}
	if v, _ := kv.Get(2, "cache:a"); v != "new" {
		t.Errorf("第 2 库 cache:a = %q", v)
	}
	done := dev.get("/sqlworkflow/" + id)
	for _, want := range []string{"已正常结束", "执行前的原值", "old", "(integer) 0"} {
		if !strings.Contains(done.Body.String(), want) {
			t.Errorf("结束后的详情页缺少 %q", want)
		}
	}
}

func TestSQLWorkflowRedisMySQLUnaffected(t *testing.T) {
	// 没有装配规则来源、也没有 Redis 实例时，原有页面照常（提交页不依赖 Redis）
	e := newSQLEnv(t)
	dev := e.login(t, "dev")
	if w := dev.get("/sqlworkflow/new"); w.Code != 200 || !strings.Contains(w.Body.String(), "wf-redis-note") {
		t.Errorf("提交页: %d", w.Code)
	}
	w := dev.do("POST", "/api/v1/sqlworkflow/check", map[string]any{"instance_id": 1, "db_name": "shop", "sql": "update orders set a=1 where id=1;"})
	if d := dataOf(t, w); w.Code != 200 || d["source"] != "native" {
		t.Errorf("MySQL 检查: %d %s", w.Code, w.Body.String())
	}
}
