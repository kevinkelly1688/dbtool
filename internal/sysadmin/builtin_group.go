// @author: kevinkelly
// @license: Apache Licence
// @file: builtin_group.go
// @time: 2026/10/28

package sysadmin

import (
	"log/slog"
)

// 内置权限组（dbt_auth_group_ext.builtin=1，deploy/sql/150_builtin_groups.sql 把名称恰为 DBA / TL / RD / QA / Default 的组标成内置）：
// 不能改名、不能删除；权限码和说明照常可改。校验在服务端，页面只是同步显示。
// 旁路表或 builtin 列不存在（还没执行 150）时没有内置组，所有权限组按普通组处理，不报错。

// builtinGroups 返回给定权限组里的内置组；读取失败时记日志并按“没有内置组”处理（不因为旁路表问题挡住管理员操作）。
func builtinGroups(x ExtRepo, ids []uint) map[uint]bool {
	m, err := x.AuthGroupBuiltins(ids)
	if err != nil {
		slog.Warn("读取内置权限组标记失败，按普通组处理", "err", err)
		return map[uint]bool{}
	}
	return m
}

func isBuiltinGroup(x ExtRepo, id uint) bool { return builtinGroups(x, []uint{id})[id] }

// errBuiltinRename / errBuiltinDelete 是对内置组改名、删除时的拒绝提示。
func errBuiltinRename(name string) error {
	return forbid(errf("「%s」是内置权限组，不能改名（权限和说明可以修改）", name))
}

func errBuiltinDelete(name string) error {
	return forbid(errf("「%s」是内置权限组，不能删除（可以调整它的权限；不再使用时请移除成员）", name))
}

// builtinNames 是 150_builtin_groups.sql 标成内置的权限组名称，仅用于测试对照 SQL 脚本。
var builtinNames = []string{"DBA", "TL", "RD", "QA", "Default"}
