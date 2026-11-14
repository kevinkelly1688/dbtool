// @author: kevinkelly
// @license: Apache Licence
// @file: grants_test.go
// @time: 2026/10/28

package server_test

// 夹具里授权用的按钮组合：一个“角色”要的几个按钮放在一起，写夹具时按角色拼。
var (
	grantViewOwn   = []string{"sqlworkflow.view_own"}
	grantSubmit    = []string{"sqlworkflow.submit"}
	grantExecute   = []string{"sqlworkflow.execute_own", "sqlworkflow.schedule", "sqlworkflow.manual_finish", "sqlworkflow.osc", "sqlworkflow.batch_op"}
	grantExecGroup = []string{"sqlworkflow.execute_group", "sqlworkflow.cancel_run"}
	grantReview    = []string{"sqlworkflow.view_group", "sqlworkflow.approve", "sqlworkflow.reject", "sqlworkflow.alter_window"}
)

// grants 把按钮组合合成 memrepo.Perms 里一个用户的授权集合。
func grants(groups ...[]string) map[string]bool {
	out := map[string]bool{}
	grantTo(out, groups...)
	return out
}

// grantTo 给已有的授权集合追加按钮组合。
func grantTo(m map[string]bool, groups ...[]string) {
	for _, g := range groups {
		for _, c := range g {
			m[c] = true
		}
	}
}
