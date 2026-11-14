// @author: kevinkelly
// @license: Apache Licence
// @file: export.go
// @time: 2026/10/28

package query

import "dbtool/internal/model"

// 以下是供权限申请等模块复用的导出封装，保证“是否已有权限”“实例标签”等判断与查询流水线一致。

// NewError 创建对外可见的业务错误。
func NewError(status int, code, msg string) *Error { return newErr(status, code, msg) }

// HasDBPriv 判断用户有效权限里是否已有某个库的库权限（库名相同或 *）。
func HasDBPriv(privs []model.QueryPrivilege, db string) bool {
	_, ok := dbPriv(privs, db)
	return ok
}

// HasTablePriv 判断用户有效权限里是否已有某张表的表权限。
func HasTablePriv(privs []model.QueryPrivilege, db, table string) bool {
	_, ok := tbPriv(privs, db, table)
	return ok
}

// HasActiveTag 判断实例是否带有任一启用的标签（按标签代码）。
func HasActiveTag(inst *model.Instance, codes ...string) bool { return hasActiveTag(inst, codes...) }
