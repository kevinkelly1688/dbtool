// @author: kevinkelly
// @license: Apache Licence
// @file: settingsguard_test.go
// @time: 2026/10/28

package sqlopt

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"dbtool/internal/model"
)

func writeFile(t *testing.T, dir, name string, mode os.FileMode, size int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, make([]byte, size), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // 不受 umask 影响
		t.Fatal(err)
	}
	return p
}

func wantErrHas(t *testing.T, what string, err error, sub string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), sub) {
		t.Errorf("%s：期望错误含 %q，得到 %v", what, sub, err)
	}
}

func TestGuardSoarProgram(t *testing.T) {
	dir := t.TempDir()
	unix := runtime.GOOS != "windows"

	ok := writeFile(t, dir, "soar", 0o755, 10)
	if err := guardSoarProgram(ok); err != nil {
		t.Errorf("正常的程序文件应通过: %v", err)
	}
	wantErrHas(t, "不存在", guardSoarProgram(filepath.Join(dir, "nope")), "不存在")
	wantErrHas(t, "目录", guardSoarProgram(dir), "不是普通文件")

	if unix {
		wantErrHas(t, "无执行权限", guardSoarProgram(writeFile(t, dir, "soar-noexec", 0o644, 1)), "执行权限")
		wantErrHas(t, "其他用户可写", guardSoarProgram(writeFile(t, dir, "soar-ow", 0o757, 1)), "其他用户改写")

		link := filepath.Join(dir, "soar-link")
		if err := os.Symlink(ok, link); err == nil {
			wantErrHas(t, "符号链接", guardSoarProgram(link), "符号链接")
		}

		open := filepath.Join(dir, "open")
		if err := os.Mkdir(open, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(open, 0o777); err != nil {
			t.Fatal(err)
		}
		wantErrHas(t, "目录可写", guardSoarProgram(writeFile(t, open, "soar", 0o755, 1)), "所在目录")
	}
}

func TestGuardSoarConfigFile(t *testing.T) {
	dir := t.TempDir()
	if err := guardSoarConfigFile(writeFile(t, dir, "soar.yaml", 0o644, 100)); err != nil {
		t.Errorf("正常的配置文件应通过: %v", err)
	}
	wantErrHas(t, "太大", guardSoarConfigFile(writeFile(t, dir, "big.yaml", 0o644, maxSoarConfigBytes+1)), "1MB")
	wantErrHas(t, "不存在", guardSoarConfigFile(filepath.Join(dir, "nope.yaml")), "不存在")
	wantErrHas(t, "目录", guardSoarConfigFile(dir), "不是普通文件")
}

func TestGuardSoarTestDSN(t *testing.T) {
	insts := []model.Instance{
		{ID: 1, InstanceName: "线上主库", Host: "10.1.1.1", Port: 3306, Tags: []model.InstanceTag{{TagCode: "prod", TagName: "生产"}}},
		{ID: 2, InstanceName: "测试库", Host: "10.2.2.2", Port: 3306, Tags: []model.InstanceTag{{TagCode: "test", TagName: "测试"}}},
		{ID: 3, InstanceName: "没标环境", Host: "10.3.3.3", Port: 3307},
		{ID: 4, InstanceName: "别名主库", Host: "prod-alias.example.com", Port: 3306, Tags: []model.InstanceTag{{TagCode: "prod", TagName: "生产"}}},
	}
	resolve := func(h string) []string {
		switch h {
		case "prod-alias.example.com", "db-a.example.com":
			return []string{"10.1.1.1"}
		}
		if net.ParseIP(h) != nil { // 字面 IP 原样
			return []string{h}
		}
		return nil
	}
	list := func() ([]model.Instance, error) { return insts, nil }
	g := guardSoarTestDSN(list, resolve)

	cases := []struct {
		dsn string
		err string // 空 = 通过
	}{
		{"u:p@10.9.9.9:3306/t", ""},              // 没登记的独立测试库
		{"u:p@10.2.2.2:3306/t", ""},              // 登记为测试环境
		{"u:p@10.1.1.1:3306/t", "生产实例"},          // 登记的生产库
		{"u:p@10.1.1.1/t", "生产实例"},               // 缺省端口 3306
		{"u:p@10.1.1.1:3307/t", ""},              // 端口不同不是同一个服务
		{"u:p@db-a.example.com:3306/t", "线上主库"},  // 别名解析到生产库的地址
		{"u:p@PROD-ALIAS.example.com/t", "别名主库"}, // 主机名按文字比较时不区分大小写
		{"u:p@10.3.3.3:3307/t", "环境没有设置"},        // 没标环境，证明不了不是生产
		{"nonsense", "格式"},                       // 写法不对
	}
	for _, c := range cases {
		err := g(c.dsn)
		if c.err == "" && err != nil || c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s：期望 %q，得到 %v", c.dsn, c.err, err)
		}
	}

	// 读不到实例清单时拒绝（宁严勿松）
	bad := guardSoarTestDSN(func() ([]model.Instance, error) { return nil, errors.New("db down") }, resolve)
	wantErrHas(t, "清单读取失败", bad("u:p@10.9.9.9:3306/t"), "无法确认")
}
