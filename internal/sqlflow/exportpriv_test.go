// @author: kevinkelly
// @license: Apache Licence
// @file: exportpriv_test.go
// @time: 2026/10/28

package sqlflow_test

import (
	"context"
	"strings"
	"testing"

	"dbtool/internal/model"
	"dbtool/internal/query"
	"dbtool/internal/sqlflow/memrepo"
)

// qprivRepo 给导出工单的权限校验提供查询权限（dbt_query_privileges）。
type qprivRepo struct {
	*memrepo.Repo
	privs map[uint][]model.QueryPrivilege // 实例 ID -> 有效的查询权限
}

func (r qprivRepo) ValidQueryPrivileges(_ string, inst uint) ([]model.QueryPrivilege, error) {
	return r.privs[inst], nil
}
func (r qprivRepo) InsertQueryLog(*model.QueryLog) error { return nil }

func exportPrivEnv(t *testing.T) *exportEnv {
	t.Helper()
	xe := setupExport(t)
	qr := qprivRepo{Repo: xe.repo, privs: map[uint][]model.QueryPrivilege{
		1:  {{PrivType: 2, DBName: "shop", TbName: "orders", LimitNum: 100}}, // MySQL：只有 shop.orders 的表权限
		11: nil,                                                              // SQL Server 11：没有任何权限
		12: {{PrivType: 1, DBName: "ShopDB", LimitNum: 100}},                 // SQL Server 12：ShopDB 库权限
	}}
	xe.svc.SetExportAuthorizer(query.NewPrivAuthorizer(qr, nil))
	return xe
}

// 导出页的实例与库列表只显示有查询权限的；SQL 上线的实例与库列表不受影响。
func TestExportListsOnlyPermittedInstancesAndDatabases(t *testing.T) {
	xe := exportPrivEnv(t)
	ctx := context.Background()
	o, err := xe.svc.SubmitOptions(dev)
	if err != nil {
		t.Fatal(err)
	}
	var ids []uint
	for _, in := range o.ExportInstances[1] {
		ids = append(ids, in.ID)
	}
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 12 {
		t.Errorf("导出实例只应有 1（表权限）和 12（库权限）: %v", ids)
	}
	// 上线实例列表不变
	var up []uint
	for _, in := range o.Instances[1] {
		up = append(up, in.ID)
	}
	if len(up) != 1 || up[0] != 1 {
		t.Errorf("上线工单的实例列表不应受影响: %v", up)
	}
	// 库：MySQL 只有 shop（有 shop.orders 表权限）；SQL Server 12 只有 ShopDB
	dbs, err := xe.svc.ExportDatabases(ctx, dev, 1)
	if err != nil || len(dbs) != 1 || dbs[0] != "shop" {
		t.Errorf("导出库列表: %v %v", dbs, err)
	}
	dbs, err = xe.svc.ExportDatabases(ctx, dev, 12)
	if err != nil || len(dbs) != 1 || dbs[0] != "ShopDB" {
		t.Errorf("SQL Server 导出库列表: %v %v", dbs, err)
	}
	// 普通的库列表（上线用）不受影响
	if all, err := xe.svc.Databases(ctx, dev, 1); err != nil || len(all) != 2 {
		t.Errorf("上线工单的库列表不应被过滤: %v %v", all, err)
	}
	// 超级用户不受限
	if all, err := xe.svc.ExportDatabases(ctx, root, 1); err != nil || len(all) != 2 {
		t.Errorf("超级用户: %v %v", all, err)
	}
}

// 提交 / 检查时校验语句涉及的表有没有权限。
func TestExportCheckRequiresTablePrivileges(t *testing.T) {
	xe := exportPrivEnv(t)
	ctx := context.Background()
	ok, err := xe.svc.PreviewExport(ctx, dev, []uint{1}, false, "shop", "select id, name from orders where id > 0 order by id")
	if err != nil || ok.ErrorCount != 0 {
		t.Fatalf("有 shop.orders 表权限应通过: %+v %v", ok, err)
	}
	bad, err := xe.svc.PreviewExport(ctx, dev, []uint{1}, false, "shop", "select id from secrets where id > 0")
	if err != nil || bad.ErrorCount != 1 || !strings.Contains(bad.Rows[0].ErrorMessage, "shop.secrets") || !strings.Contains(bad.Rows[0].ErrorMessage, "查询权限") {
		t.Fatalf("没有 shop.secrets 的权限应报错: %+v %v", bad, err)
	}
	// 联表里有一张没权限的表也不行
	bad, _ = xe.svc.PreviewExport(ctx, dev, []uint{1}, false, "shop", "select o.id from orders o join secrets s on s.id = o.id where o.id > 0")
	if bad.ErrorCount != 1 || !strings.Contains(bad.Rows[0].ErrorMessage, "shop.secrets") {
		t.Errorf("联表: %+v", bad.Rows)
	}
	// 库级权限（SQL Server）：库里任何表都行
	if r, err := xe.svc.PreviewExport(ctx, dev, []uint{12}, false, "ShopDB", "select id from dbo.Orders where id > 0 order by id"); err != nil || r.ErrorCount != 0 {
		t.Errorf("SQL Server 库权限: %+v %v", r, err)
	}
	// 没有任何权限的实例
	if r, _ := xe.svc.PreviewExport(ctx, dev, []uint{11}, false, "ShopDB", "select id from dbo.Orders where id > 0 order by id"); r.ErrorCount != 1 || !strings.Contains(r.Rows[0].ErrorMessage, "ShopDB") {
		t.Errorf("无权限的实例: %+v", r.Rows)
	}
	// 批量导出：每个实例各自校验，错误行标明实例
	r, _ := xe.svc.PreviewExport(ctx, dev, []uint{11, 12}, true, "ShopDB", "select id from dbo.Orders where id > 0 order by id")
	if r.ErrorCount != 1 || !strings.Contains(r.Rows[0].ErrorMessage, "mssql-11") {
		t.Errorf("批量：只有 11 没权限: %+v", r.Rows)
	}
	// 提交同样被拒绝
	before := len(xe.repo.Workflows)
	if _, err := xe.svc.Submit(ctx, dev, xe.req("select id from secrets where id > 0", "csv", 1)); err == nil || code(err) != "forbidden" || !strings.Contains(err.Error(), "shop.secrets") {
		t.Errorf("没有表权限的导出不能提交（也不产生工单）: %v", err)
	}
	if len(xe.repo.Workflows) != before {
		t.Errorf("被拒绝的提交不应产生工单")
	}
	// 有权限的表可以提交
	if _, err := xe.svc.Submit(ctx, dev, xe.req("select id, name from orders where id > 0 order by id", "csv", 1)); err != nil {
		t.Errorf("有权限的导出应能提交: %v", err)
	}
	// 超级用户不受限
	if r, err := xe.svc.PreviewExport(ctx, root, []uint{1}, false, "shop", "select id from secrets where id > 0"); err != nil || r.ErrorCount != 0 {
		t.Errorf("超级用户不校验库表权限: %+v %v", r, err)
	}
}
