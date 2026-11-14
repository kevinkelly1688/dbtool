// @author: kevinkelly
// @license: Apache Licence
// @file: envdict_test.go
// @time: 2026/10/28

package instext

import (
	"strings"
	"testing"
)

// 没有绑定元数据库（或字典表不可用）时，环境只有内置的生产 / 测试，行为与引入字典之前一致。
func TestEnvBuiltinFallback(t *testing.T) {
	BindEnvDB(nil)
	defs := EnvDefs()
	if len(defs) != 2 || defs[0].Code != EnvProd || defs[1].Code != EnvTest {
		t.Fatalf("内置环境 = %+v", defs)
	}
	for _, c := range []struct {
		code  string
		valid bool
		label string
		prod  bool
	}{
		{"", true, "", false},
		{"prod", true, "生产", true},
		{"test", true, "测试", false},
		{"staging", false, "staging", false}, // 字典里没有：不合法；显示名回落到编码
	} {
		if got := ValidEnv(c.code); got != c.valid {
			t.Errorf("ValidEnv(%q)=%v", c.code, got)
		}
		if got := EnvLabel(c.code); got != c.label {
			t.Errorf("EnvLabel(%q)=%q, want %q", c.code, got, c.label)
		}
		if got := EnvIsProd(c.code); got != c.prod {
			t.Errorf("EnvIsProd(%q)=%v", c.code, got)
		}
	}
}

// 实例有效环境：没有绑定库 / 没设置时用调用方给的回退值（由标签推断）。
func TestInstanceEnvOfFallback(t *testing.T) {
	BindEnvDB(nil)
	if got := InstanceEnvOf(7, "prod"); got != "prod" {
		t.Errorf("回退 = %q", got)
	}
	if got := InstanceEnvOf(7, ""); got != "" {
		t.Errorf("回退 = %q", got)
	}
}

func TestValidateEnvInput(t *testing.T) {
	bad := []struct{ code, name, why string }{
		{"Prod", "生产", "大写"},
		{"1abc", "x", "数字开头"},
		{"a b", "x", "空格"},
		{"all", "全部", "保留字"},
		{strings.Repeat("a", 17), "x", "超长"},
		{"staging", "", "名称为空"},
		{"staging", strings.Repeat("长", 21), "名称超长"},
	}
	for _, c := range bad {
		if _, _, err := ValidateEnvInput(c.code, c.name, true); err == nil {
			t.Errorf("%s：应拒绝 %q / %q", c.why, c.code, c.name)
		}
	}
	code, name, err := ValidateEnvInput(" staging ", " 预发 ", true)
	if err != nil || code != "staging" || name != "预发" {
		t.Errorf("合法输入 = %q %q %v", code, name, err)
	}
	// 修改时不校验编码（路径里的编码已存在）
	if _, _, err := ValidateEnvInput("whatever", "预发", false); err != nil {
		t.Errorf("修改不应校验编码：%v", err)
	}
}
