// @author: kevinkelly
// @license: Apache Licence
// @file: exportpriv.go
// @time: 2026/10/28

package sqlflow

import (
	"context"
	"fmt"

	"dbtool/internal/audit"
	"dbtool/internal/model"
	"dbtool/internal/query"
)

// 数据导出的库表权限（设计：与在线查询共用查询权限 dbt_query_privileges）：
//   - 列表里只显示用户有权限的实例和库（没有权限的不显示）；
//   - 提交 / 检查时校验语句里涉及的库表都有权限，没有就在检查结果里报错。
// 超级用户、查询全部实例、资源组管理员不受限。不调用 SetExportAuthorizer（测试 / 演示）时不做这些限制。
// SQL 上线工单不受影响。

// SetExportAuthorizer 接入数据导出的库表权限校验；传 nil = 不校验。
func (s *Service) SetExportAuthorizer(a *query.PrivAuthorizer) { s.exportAuth = a }

func queryPrincipal(p audit.Actor) query.Principal {
	return query.Principal{ID: p.ID, Username: p.Username, Display: p.Display, IsSuperuser: p.IsSuperuser}
}

// exportVisible 判断实例是否出现在该用户的导出实例列表里（结果按实例缓存在 memo 里，同一个实例属于多个资源组时不重复查）。
func (s *Service) exportVisible(p audit.Actor, in *model.Instance, memo map[uint]bool) (bool, error) {
	if s.exportAuth == nil {
		return true, nil
	}
	if v, ok := memo[in.ID]; ok {
		return v, nil
	}
	sc, err := s.exportAuth.ExportScope(queryPrincipal(p), in)
	if err != nil {
		return false, err
	}
	memo[in.ID] = sc.HasAny()
	return sc.HasAny(), nil
}

// ExportDatabases 同 Databases，但只返回用户有库权限或表权限的库（导出页用）。
func (s *Service) ExportDatabases(ctx context.Context, p audit.Actor, instID uint) ([]string, error) {
	dbs, err := s.Databases(ctx, p, instID)
	if err != nil || s.exportAuth == nil {
		return dbs, err
	}
	inst, err := s.repo.GetInstance(instID)
	if err != nil || inst == nil {
		return dbs, err
	}
	sc, err := s.exportAuth.ExportScope(queryPrincipal(p), inst)
	if err != nil {
		return nil, err
	}
	if sc.All {
		return dbs, nil
	}
	out := make([]string, 0, len(dbs))
	for _, d := range dbs {
		if sc.HasDB(d) {
			out = append(out, d)
		}
	}
	return out, nil
}

// exportDenied 逐个实例校验导出语句涉及的库表权限，返回 实例下标 -> 拒绝原因（有权限的不在结果里）。
func (s *Service) exportDenied(ctx context.Context, p audit.Actor, insts []*model.Instance, db, stmt string) map[int]string {
	if s.exportAuth == nil {
		return nil
	}
	var out map[int]string
	qp := queryPrincipal(p)
	for i, in := range insts {
		if err := s.exportAuth.AuthorizeExport(ctx, qp, in, db, stmt); err != nil {
			if out == nil {
				out = map[int]string{}
			}
			out[i] = errText(err)
			if _, ok := err.(*query.Error); !ok {
				out[i] = fmt.Sprintf("无法校验导出权限：%s", errText(err))
			}
		}
	}
	return out
}
