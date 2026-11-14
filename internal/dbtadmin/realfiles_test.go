// @author: kevinkelly
// @license: Apache Licence
// @file: realfiles_test.go
// @time: 2026/10/28

package dbtadmin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 仓库里真实的 deploy/bootstrap 与 deploy/sql：只验证本工具能把它们切分并“演练 + 假库应用”走通（不连真实 MySQL）。
// 目录不存在（比如只拿到了 internal/ 的片段）时跳过。
func TestRealDeployScriptsSplitAndRun(t *testing.T) {
	root := filepath.Join("..", "..", "deploy")
	if _, err := os.Stat(filepath.Join(root, "sql")); err != nil {
		t.Skip("没有 deploy/sql")
	}
	st := newMemStore()
	te := newTestEnv(t, st)
	te.env["DATABASE_URL"] = testDBURL
	args := []string{"bootstrap", "--sql-root", root, "--apply", "--yes"}
	if _, err := os.Stat(filepath.Join(root, "bootstrap")); err != nil {
		t.Log("没有 deploy/bootstrap，只验证 --only-sql")
		args = append(args, "--only-sql")
	}
	if code := te.run(args...); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, te.all())
	}
	if len(st.execs) < 50 {
		t.Errorf("语句数 %d 偏少", len(st.execs))
	}
	for _, s := range st.execs {
		if strings.TrimSpace(s) == "" {
			t.Fatal("出现空语句")
		}
	}
	t.Logf("文件 %d 个，语句 %d 条，建表 %d 张（含 %s）", len(st.applied), len(st.execs), len(st.tables), TblApplied)
}
