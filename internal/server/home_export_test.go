// @author: kevinkelly
// @license: Apache Licence
// @file: home_export_test.go
// @time: 2026/10/28

package server

// 给 package server_test 里的首页测试注入假的数据库读取器（真实环境读元数据库的 dbt_sql_workflow / dbt_audit_log）。

type HomeWorkflowRow = homeWorkflowRow
type HomeDB = homeDB

// SetHomeDB 替换首页的数据库读取器，返回还原函数。
func SetHomeDB(db HomeDB) (restore func()) {
	old := homeDBFor
	homeDBFor = func(*Server) homeDB { return db }
	return func() { homeDBFor = old }
}

// ReplaceHomeApproveHooks 用测试里的钩子替换模块登记的“待我审批”来源，返回还原函数。
func ReplaceHomeApproveHooks(hooks ...HomeApproveFunc) (restore func()) {
	old := homeApproveHooks
	homeApproveHooks = hooks
	return func() { homeApproveHooks = old }
}
