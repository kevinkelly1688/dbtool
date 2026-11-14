// @author: kevinkelly
// @license: Apache Licence
// @file: hook.go
// @time: 2026/10/28

package btnperm

import "sync/atomic"

// GroupSaveCheck 在“保存 / 预检权限组按钮”时被调用，返回额外的警告（只提示，不阻止保存）。
// 参数是权限组 ID、名称与保存后的按钮码集合；调用发生在写库之前，所以实现里可以拿“当前状态”与“假设保存后”做比较。
// 典型用途：系统管理模块检查这次保存会不会让某条审批流的某一级无人可审。
// 用钩子而不是直接引用，是因为 btnperm 不能反向依赖系统管理模块。
type GroupSaveCheck func(groupID uint, groupName string, next []string) []string

var groupSaveCheck atomic.Pointer[GroupSaveCheck]

// SetGroupSaveCheck 登记（或用 nil 取消）保存前预检。
func SetGroupSaveCheck(f GroupSaveCheck) {
	if f == nil {
		groupSaveCheck.Store(nil)
		return
	}
	groupSaveCheck.Store(&f)
}

// runGroupSaveCheck 执行已登记的预检；预检自身出错（含 panic）只会让它没有输出，不影响保存。
func runGroupSaveCheck(groupID uint, groupName string, next []string) (out []string) {
	f := groupSaveCheck.Load()
	if f == nil {
		return nil
	}
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	return (*f)(groupID, groupName, next)
}
