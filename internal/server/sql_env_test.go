// @author: kevinkelly
// @license: Apache Licence
// @file: sql_env_test.go
// @time: 2026/10/28

package server_test

import (
	"strings"
	"testing"
)

// 详情页 <main id="detail"> 输出 data-env，JSON 详情返回 env：前端据此决定生产环境的高危确认要不要键入工单号。
func TestSQLDetailOutputsInstanceEnv(t *testing.T) {
	cases := []struct {
		name  string
		env   map[uint]string
		noExt bool
		want  string
	}{
		{"生产", map[uint]string{1: "prod"}, false, "prod"},
		{"测试", map[uint]string{1: "test"}, false, "test"},
		{"无环境", nil, false, ""},
		{"扩展表不存在", map[uint]string{1: "prod"}, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newSQLEnv(t)
			e.mem.InstEnv, e.mem.NoInstExt = c.env, c.noExt
			dev := e.login(t, "dev")
			w := dev.do("POST", "/api/v1/sqlworkflow", submitBody("update orders set a=1 where id=1"))
			id := itoa(dataOf(t, w)["workflow_id"].(float64))

			page := dev.get("/sqlworkflow/" + id)
			if page.Code != 200 || !strings.Contains(page.Body.String(), `id="detail"`) {
				t.Fatalf("详情页: %d", page.Code)
			}
			if !strings.Contains(page.Body.String(), ` data-env="`+c.want+`"`) {
				t.Errorf("详情页应输出 data-env=%q", c.want)
			}
			if got, ok := dataOf(t, dev.do("GET", "/api/v1/sqlworkflow/"+id, nil))["env"]; !ok || got != c.want {
				t.Errorf("JSON env = %v (present=%v), want %q", got, ok, c.want)
			}
		})
	}
}

func TestSQLDetailBatchEnvMixedIsProd(t *testing.T) {
	e := newSQLEnv(t)
	e.mem.InstEnv = map[uint]string{11: "test", 12: "prod"}
	dev := e.login(t, "dev")
	w := dev.do("POST", "/api/v1/sqlworkflow", map[string]any{"name": "批量", "instance_ids": []int{11, 12}, "db_name": "shop", "group_name": "研发组", "sql": "update dbo.Orders set status = 1 where id = 5"})
	if w.Code != 200 {
		t.Fatalf("提交批量: %d %s", w.Code, w.Body.String())
	}
	id := itoa(dataOf(t, w)["workflow_id"].(float64))
	if !strings.Contains(dev.get("/sqlworkflow/"+id).Body.String(), ` data-env="prod"`) {
		t.Error("批量工单有任一生产实例即 prod")
	}
	if dataOf(t, dev.do("GET", "/api/v1/sqlworkflow/"+id, nil))["env"] != "prod" {
		t.Error("JSON env 应为 prod")
	}
}
