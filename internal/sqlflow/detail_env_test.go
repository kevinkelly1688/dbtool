// @author: kevinkelly
// @license: Apache Licence
// @file: detail_env_test.go
// @time: 2026/10/28

package sqlflow_test

import (
	"context"
	"testing"

	"dbtool/internal/instext"
)

// 详情带出目标实例的环境（前端据此决定生产环境的高危确认要不要键入工单号）。

func envOf(t *testing.T, e *env, id uint) string {
	t.Helper()
	d, err := e.svc.Get(dev, id)
	if err != nil {
		t.Fatal(err)
	}
	return d.Env
}

func TestDetailEnvSingleInstance(t *testing.T) {
	cases := []struct {
		name  string
		env   map[uint]string
		noExt bool
		want  string
	}{
		{"生产", map[uint]string{1: instext.EnvProd}, false, "prod"},
		{"测试", map[uint]string{1: instext.EnvTest}, false, "test"},
		{"无环境记录", map[uint]string{2: instext.EnvProd}, false, ""},
		{"环境为空", map[uint]string{1: ""}, false, ""},
		{"扩展表不存在", map[uint]string{1: instext.EnvProd}, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t)
			e.repo.InstEnv, e.repo.NoInstExt = c.env, c.noExt
			id := e.submit(t, dev, "update orders set a=1 where id=1")
			if got := envOf(t, e, id); got != c.want {
				t.Fatalf("env = %q, want %q", got, c.want)
			}
		})
	}
}

func TestDetailEnvBatch(t *testing.T) {
	cases := []struct {
		name string
		env  map[uint]string
		want string
	}{
		{"混合：任一生产即生产", map[uint]string{11: instext.EnvTest, 12: instext.EnvProd, 13: instext.EnvTest}, "prod"},
		{"全测试", map[uint]string{11: instext.EnvTest, 12: instext.EnvTest, 13: instext.EnvTest}, "test"},
		{"测试混未设置", map[uint]string{11: instext.EnvTest}, "test"},
		{"生产混未设置", map[uint]string{13: instext.EnvProd}, "prod"},
		{"全未设置", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			be := setupBatch(t)
			be.repo.InstEnv = c.env
			res, err := be.svc.Submit(context.Background(), dev, be.batchReq("update dbo.Orders set status = 1 where id = 1", 11, 12, 13))
			if err != nil {
				t.Fatal(err)
			}
			if got := envOf(t, be.env, res.WorkflowID); got != c.want {
				t.Fatalf("env = %q, want %q", got, c.want)
			}
		})
	}
}

func TestDetailEnvBatchExtTableMissing(t *testing.T) {
	be := setupBatch(t)
	be.repo.InstEnv, be.repo.NoInstExt = map[uint]string{11: instext.EnvProd}, true
	res, err := be.svc.Submit(context.Background(), dev, be.batchReq("update dbo.Orders set status = 1 where id = 1", 11, 12))
	if err != nil {
		t.Fatal(err)
	}
	if got := envOf(t, be.env, res.WorkflowID); got != "" {
		t.Fatalf("扩展表不存在时环境为空，got %q", got)
	}
}

// 详情只查一次环境（批量工单也是一次 IN 查询，不能按实例逐个查）。
func TestDetailEnvQueriedOnce(t *testing.T) {
	be := setupBatch(t)
	be.repo.InstEnv = map[uint]string{12: instext.EnvProd}
	res, err := be.svc.Submit(context.Background(), dev, be.batchReq("update dbo.Orders set status = 1 where id = 1", 11, 12, 13))
	if err != nil {
		t.Fatal(err)
	}
	be.repo.EnvCalls = 0
	if _, err := be.svc.Get(dev, res.WorkflowID); err != nil {
		t.Fatal(err)
	}
	if be.repo.EnvCalls != 1 {
		t.Fatalf("详情应只查一次环境，实际 %d 次", be.repo.EnvCalls)
	}
}
