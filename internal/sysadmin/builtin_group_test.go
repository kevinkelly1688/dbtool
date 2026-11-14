// @author: kevinkelly
// @license: Apache Licence
// @file: builtin_group_test.go
// @time: 2026/10/28

package sysadmin

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 内置权限组：不能改名、不能删除，权限与说明照常可改；有成员时删除的处理保持“自动解除成员”；
// 扩展表 / builtin 列不存在时按普通组处理。
func TestBuiltinAuthGroup(t *testing.T) {
	s, f := setup()
	f.xs().builtin[20] = true // DBA（setup 里是 20）

	// 列表与详情带出内置标记
	list, err := s.ListAuthGroups()
	if err != nil {
		t.Fatal(err)
	}
	flags := map[string]bool{}
	for _, g := range list {
		flags[g.Name] = g.Builtin
	}
	if !flags["DBA"] || flags["TL"] || flags["RD"] {
		t.Fatalf("只有 DBA 是内置: %v", flags)
	}
	if d, _ := s.GetAuthGroup(20); !d.Builtin {
		t.Fatalf("%+v", d)
	}
	if d, _ := s.GetAuthGroup(10); d.Builtin {
		t.Fatalf("%+v", d)
	}

	// 改名被拒（服务端校验），库里名字不变，不写成功审计
	n := len(f.audits)
	err = s.UpdateAuthGroup(root, 20, AuthGroupInput{Name: "DBA2"})
	wantErr(t, err, 403, "内置权限组")
	if f.authGroups[20] != "DBA" || len(f.audits) != n {
		t.Fatalf("改名应被拒绝且不留痕: %q audits=%d→%d", f.authGroups[20], n, len(f.audits))
	}
	// 改说明允许；名字不变（含前后空格被规整）也允许
	if err := s.UpdateAuthGroup(root, 20, AuthGroupInput{Name: " DBA ", Description: strp("数据库管理员")}); err != nil {
		t.Fatal(err)
	}
	if f.xs().descs[20] != "数据库管理员" {
		t.Fatalf("%+v", f.xs().descs)
	}
	if d, _ := s.GetAuthGroup(20); d.Description != "数据库管理员" || d.Name != "DBA" {
		t.Fatalf("%+v", d)
	}

	// 删除被拒
	wantErr(t, s.DeleteAuthGroup(root, 20), 403, "不能删除")
	if _, ok := f.authGroups[20]; !ok {
		t.Fatal("内置组不能被删除")
	}
	if f.userGroups[3][20] != true {
		t.Fatal("被拒绝时不能动成员关系")
	}

	// 普通组不受影响：可改名；有成员时删除仍然自动解除成员（现状不变）
	if err := s.UpdateAuthGroup(root, 10, AuthGroupInput{Name: "TL2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAuthGroup(root, 10); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.authGroups[10]; ok || f.userGroups[2][10] {
		t.Fatal("普通组有成员时删除应自动解除成员")
	}
	// 内置组不存在仍是 404，而不是 403
	wantErr(t, s.DeleteAuthGroup(root, 999), 404, "不存在")
}

// 旧库没有扩展表 / 没有 builtin 列：全部按普通组处理，不报错。
func TestBuiltinAuthGroupDegrades(t *testing.T) {
	s, f := setup()
	f.xs().builtin[20] = true

	f.xs().builtinColMissing = true
	if err := s.UpdateAuthGroup(root, 20, AuthGroupInput{Name: "DBA-new"}); err != nil {
		t.Fatalf("没有 builtin 列时不保护: %v", err)
	}
	if list, err := s.ListAuthGroups(); err != nil {
		t.Fatal(err)
	} else {
		for _, g := range list {
			if g.Builtin {
				t.Fatalf("没有 builtin 列时不应显示内置: %+v", g)
			}
		}
	}
	if err := s.DeleteAuthGroup(root, 20); err != nil {
		t.Fatalf("没有 builtin 列时不保护: %v", err)
	}

	f.xs().builtinColMissing = false
	f.xs().missing = true
	if err := s.UpdateAuthGroup(root, 30, AuthGroupInput{Name: "RD2"}); err != nil {
		t.Fatalf("扩展表不存在时不保护: %v", err)
	}
}

// 读取内置标记失败（旁路表异常）不挡管理员操作：记日志后按普通组处理。
func TestBuiltinGroupsReadErrorIsIgnored(t *testing.T) {
	m := builtinGroups(errBuiltinExt{nullExt{}}, nil)
	if len(m) != 0 {
		t.Fatalf("%v", m)
	}
}

type errBuiltinExt struct{ nullExt }

func (errBuiltinExt) AuthGroupBuiltins([]uint) (map[uint]bool, error) {
	return nil, errExtMissing
}

// store：builtin 列存在时只查 builtin=1 的组；表不存在 / 没有该列时返回空集合且不报错。
func TestStoreAuthGroupBuiltins(t *testing.T) {
	resetReadyCache()
	defer resetReadyCache()
	s, d := newRecStore(t)
	d.reset()
	if _, err := s.AuthGroupBuiltins(nil); err != nil {
		t.Fatal(err)
	}
	if !d.has("builtin = 1") {
		t.Errorf("应只取内置组: %v", d.logs)
	}
	d.reset()
	if _, err := s.AuthGroupBuiltins([]uint{3, 4}); err != nil || !d.has("group_id IN") {
		t.Errorf("%v %v", err, d.logs)
	}

	t.Run("缺表", func(t *testing.T) {
		resetReadyCache()
		s2, d2 := newMissingStore(t, "dbt_auth_group_ext")
		if m, err := s2.AuthGroupBuiltins(nil); err != nil || len(m) != 0 {
			t.Fatalf("缺表: %v %v", m, err)
		}
		if d2.count("builtin = 1") != 0 {
			t.Errorf("缺表时不应再查 builtin: %v", d2.logs)
		}
	})
	t.Run("缺列", func(t *testing.T) {
		resetReadyCache()
		s3, d3 := newMissingStore(t, "`builtin`")
		if m, err := s3.AuthGroupBuiltins(nil); err != nil || len(m) != 0 {
			t.Fatalf("缺列: %v %v", m, err)
		}
		if d3.count("builtin = 1") != 0 {
			t.Errorf("缺列时不应再查 builtin: %v", d3.logs)
		}
		s3.AuthGroupBuiltins(nil)
		if d3.count("SELECT `builtin` FROM `dbt_auth_group_ext` LIMIT 1") != 1 {
			t.Errorf("列探测结果应缓存: %v", d3.logs)
		}
	})
}

// 150_builtin_groups.sql：幂等（不依赖 ADD COLUMN IF NOT EXISTS），并把 DBA / TL / RD / QA / Default 标成内置。
func TestBuiltinGroupsSQLFile(t *testing.T) {
	b, err := os.ReadFile("../../deploy/sql/150_builtin_groups.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(b)
	for _, want := range []string{"information_schema.columns", "PREPARE", "ADD COLUMN builtin", "ON DUPLICATE KEY UPDATE builtin = 1", "FROM dbt_auth_group"} {
		if !strings.Contains(sql, want) {
			t.Errorf("缺少 %q", want)
		}
	}
	if strings.Contains(strings.ToUpper(sql), "ADD COLUMN IF NOT EXISTS") {
		t.Error("MySQL 5.7 不支持 ADD COLUMN IF NOT EXISTS")
	}
	for _, n := range builtinNames {
		if !strings.Contains(sql, "''"+n+"''") {
			t.Errorf("内置组 %s 没写进脚本", n)
		}
	}
	for _, bad := range []string{"DROP ", "DELETE "} {
		if strings.Contains(strings.ToUpper(sql), strings.ToUpper(bad)) {
			t.Errorf("脚本不应包含 %q", bad)
		}
	}
	// 不改基础表 dbt_auth_group 的结构；注意要用词边界，扩展表 dbt_auth_group_ext 的前缀与它相同。
	if regexp.MustCompile("(?i)ALTER\\s+TABLE\\s+`?dbt_auth_group(?:[^A-Za-z0-9_]|$)").MatchString(sql) {
		t.Error("脚本不应 ALTER 基础表 dbt_auth_group（只允许改扩展表 dbt_auth_group_ext）")
	}
}
