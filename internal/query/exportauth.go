// @author: kevinkelly
// @license: Apache Licence
// @file: exportauth.go
// @time: 2026/10/28

package query

import (
	"context"
	"fmt"
	"strings"

	"dbtool/internal/model"
	"dbtool/internal/sqlx"
)

// 数据导出工单对“查询权限”的校验（dbt_query_privileges，与在线查询用同一份权限）：
//
//   - 列表：只显示用户有权限的实例与库（ExportScope）；
//   - 提交：校验语句里涉及的库表用户都有权限（AuthorizeExport）。
//
// 规则与 AuthorizeQuery 的库表校验一致——超级用户、查询全部实例（query.admin_run）、资源组管理员（query.group_run，
// 且实例开启查询）不受限；其余用户逐表要求库权限或表权限（SQL Server 只看库）。区别只有两点：不要求 query.run
// （导出有自己的提交权限 export.submit），也不涉及行数上限（导出有自己的行数阈值）。

// ExportScope 是用户在某个实例上能导出的范围。
type ExportScope struct {
	All bool            // 不受库表权限限制
	DBs map[string]bool // 有库权限或表权限的库（小写）；All 为 false 时才有意义
}

// HasAny 报告范围是否非空（实例该不该出现在列表里）。
func (s ExportScope) HasAny() bool { return s.All || len(s.DBs) > 0 }

// HasDB 报告库是否在范围内。
func (s ExportScope) HasDB(db string) bool { return s.All || s.DBs[strings.ToLower(db)] }

// unrestricted 判断用户在该实例上是否不受库表权限限制。
func (a *PrivAuthorizer) unrestricted(p Principal, inst *model.Instance) (bool, error) {
	if p.IsSuperuser {
		return true, nil
	}
	admin, err := a.isAdmin(p)
	if err != nil || admin {
		return admin, err
	}
	ok, err := a.repo.HasButton(p.ID, permQueryResourceGroupIn)
	if err != nil {
		return false, err
	}
	return ok && HasReadTag(inst), nil
}

// ExportScope 返回用户在实例上的导出范围。没有任何有效权限时范围为空（HasAny 为 false）。
func (a *PrivAuthorizer) ExportScope(p Principal, inst *model.Instance) (ExportScope, error) {
	free, err := a.unrestricted(p, inst)
	if err != nil {
		return ExportScope{}, err
	}
	if free {
		return ExportScope{All: true}, nil
	}
	privs, err := a.repo.ValidQueryPrivileges(p.Username, inst.ID)
	if err != nil {
		return ExportScope{}, err
	}
	sc := ExportScope{DBs: map[string]bool{}}
	for _, pr := range privs {
		switch pr.PrivType {
		case 1: // 库权限；库名 * 表示全部库
			if pr.DBName == "*" {
				return ExportScope{All: true}, nil
			}
			sc.DBs[strings.ToLower(pr.DBName)] = true
		case 2: // 表权限：库里有可导出的表，库就该出现在列表里
			if mysqlTableLevel(inst.DBType) {
				sc.DBs[strings.ToLower(pr.DBName)] = true
			}
		}
	}
	return sc, nil
}

// AuthorizeExport 校验导出语句涉及的库表权限；没有权限返回 403 业务错误（文案与在线查询一致）。
func (a *PrivAuthorizer) AuthorizeExport(ctx context.Context, p Principal, inst *model.Instance, db, stmt string) error {
	if err := a.CanUseInstance(p, inst); err != nil {
		return err
	}
	free, err := a.unrestricted(p, inst)
	if err != nil {
		return err
	}
	if free {
		return nil
	}
	privs, err := a.repo.ValidQueryPrivileges(p.Username, inst.ID)
	if err != nil {
		return err
	}
	var refs []sqlx.TableRef
	switch {
	case mysqlTableLevel(inst.DBType):
		if refs, err = a.tableExtractorFor(inst.DBType).Tables(ctx, inst, db, stmt); err != nil {
			return err
		}
	case isMSSQL(inst.DBType):
		refs, _ = mssqlTables(stmt, db) // 一/二部分名称属于当前库，三部分名称取其库名；按库校验
	default:
		refs, _ = LexicalExtractor{}.Tables(ctx, inst, db, stmt)
	}
	if len(refs) == 0 {
		// 没有任何表的查询（SELECT 1）：至少要求对所选的库有权限，免得借此探测任意库
		if _, ok := dbPriv(privs, db); !ok && !hasTablePrivIn(privs, db) {
			return noPriv(fmt.Sprintf("你无 %s 数据库的查询权限，请先申请。", db), db, "")
		}
		return nil
	}
	tableLevel := mysqlTableLevel(inst.DBType)
	seen := map[string]bool{}
	for _, r := range refs {
		key := strings.ToLower(r.Schema)
		if tableLevel {
			key += "." + strings.ToLower(r.Name)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		_, hasDB := dbPriv(privs, r.Schema)
		hasTB := false
		if tableLevel {
			_, hasTB = tbPriv(privs, r.Schema, r.Name)
		}
		if hasDB || hasTB {
			continue
		}
		if tableLevel {
			return noPriv(fmt.Sprintf("你无 %s.%s 表的查询权限，请先申请。", r.Schema, r.Name), r.Schema, r.Name)
		}
		return noPriv(fmt.Sprintf("你无 %s 数据库的查询权限，请先申请。", r.Schema), r.Schema, "")
	}
	return nil
}

func hasTablePrivIn(privs []model.QueryPrivilege, db string) bool {
	for _, p := range privs {
		if p.PrivType == 2 && strings.EqualFold(p.DBName, db) {
			return true
		}
	}
	return false
}
