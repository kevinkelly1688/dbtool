// @author: kevinkelly
// @license: Apache Licence
// @file: exec_test.go
// @time: 2026/10/28

package giconfig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBuildArgv(t *testing.T) {
	d := DefaultDeploy()
	ok := []struct {
		name string
		mut  func(d *Deploy)
		want string
	}{
		{"docker", func(d *Deploy) { d.Container = "goinception_1" }, "docker restart goinception_1"},
		{"docker+sudo", func(d *Deploy) { d.Container, d.UseSudo = "gi", true }, "sudo -n docker restart gi"},
		{"容器名带点和横线", func(d *Deploy) { d.Container = "go-inception.v1_a" }, "docker restart go-inception.v1_a"},
		{"容器名以数字开头", func(d *Deploy) { d.Container = "1gi" }, "docker restart 1gi"},
		{"容器名 128 位", func(d *Deploy) { d.Container = strings.Repeat("a", 128) }, "docker restart " + strings.Repeat("a", 128)},
	}
	for _, c := range ok {
		x := d
		c.mut(&x)
		argv, err := BuildArgv(x)
		if err != nil {
			t.Errorf("%s：%v", c.name, err)
			continue
		}
		if got := strings.Join(argv, " "); got != c.want {
			t.Errorf("%s：%q != %q", c.name, got, c.want)
		}
	}
	bad := []struct {
		name string
		c    string
	}{
		{"容器名带斜杠", "../x"}, {"容器名当选项", "-v"}, {"容器名当选项（长）", "--help"}, {"容器名以点开头", ".x"}, {"容器名以下划线开头", "_x"},
		{"容器名带空格", "a b"}, {"容器名注入", "gi; reboot"}, {"容器名换行", "gi\nreboot"}, {"容器名尾部换行", "gi\n"}, {"容器名命令替换", "$(reboot)"},
		{"容器名反引号", "`id`"}, {"容器名引号", "a'b"}, {"容器名中文", "容器"}, {"容器名过长", strings.Repeat("a", 129)}, {"容器名前后空白", " gi "},
	}
	for _, c := range bad {
		x := d
		x.Container = c.c
		if argv, err := BuildArgv(x); err == nil {
			t.Errorf("%s：应拒绝，得到 %v", c.name, argv)
		} else if errors.Is(err, ErrNoContainer) {
			t.Errorf("%s：非法容器名不应报“未设置”：%v", c.name, err)
		}
	}
	// 没有设置容器名：不是“非法”，而是 ErrNoContainer；即使勾了 sudo 也不会生成命令
	for _, sudo := range []bool{false, true} {
		x := d
		x.UseSudo = sudo
		if argv, err := BuildArgv(x); !errors.Is(err, ErrNoContainer) || argv != nil {
			t.Errorf("容器名为空应返回 ErrNoContainer：%v %v", argv, err)
		}
	}
}

// recRunner 记录被执行的 argv，不执行任何命令。
type recRunner struct {
	argvs [][]string
	err   error
}

func (r *recRunner) Run(ctx context.Context, argv []string) (string, error) {
	r.argvs = append(r.argvs, argv)
	return "ok", r.err
}

func TestRestarterRunsDockerRestart(t *testing.T) {
	rr := &recRunner{}
	rs := execRestarter{runner: rr}
	d := DefaultDeploy()
	d.Container = "goinception"
	out, err := rs.Restart(bg, d)
	if err != nil || out != "ok" || len(rr.argvs) != 1 || strings.Join(rr.argvs[0], " ") != "docker restart goinception" {
		t.Fatalf("%v %v %q", rr.argvs, err, out)
	}
	if p, err := rs.Preview(d); err != nil || p != "docker restart goinception" {
		t.Fatal(p, err)
	}
	// sudo：argv 是 [sudo -n docker restart <容器>]，每一项都是独立参数
	rr.argvs = nil
	d.UseSudo = true
	if _, err := rs.Restart(bg, d); err != nil || fmt.Sprint(rr.argvs) != "[[sudo -n docker restart goinception]]" || len(rr.argvs[0]) != 5 {
		t.Fatalf("%v %v", rr.argvs, err)
	}
	if p, _ := rs.Preview(d); p != "sudo -n docker restart goinception" {
		t.Fatal(p)
	}
	// 命令失败：错误与输出原样交回
	rr.err = errors.New("exit status 1")
	if out, err := rs.Restart(bg, d); err == nil || out != "ok" {
		t.Fatalf("%q %v", out, err)
	}
	// 非法的容器名不会走到执行层；没有设置容器名同理（Preview 也不生成命令）
	for _, c := range []string{"x;reboot", "-rf", ""} {
		rr.argvs = nil
		d.Container = c
		if _, err := rs.Restart(bg, d); err == nil || len(rr.argvs) != 0 {
			t.Fatalf("容器名 %q 不应执行：%v %v", c, err, rr.argvs)
		}
		if p, err := rs.Preview(d); err == nil || p != "" {
			t.Fatalf("容器名 %q 不应生成预览：%q %v", c, p, err)
		}
	}
	d.Container = ""
	if _, err := rs.Restart(bg, d); !errors.Is(err, ErrNoContainer) {
		t.Fatal(err)
	}
}

func TestRestartDesc(t *testing.T) {
	d := DefaultDeploy()
	if got := restartDesc(d); got != "docker restart（未设置容器名）" {
		t.Fatal(got)
	}
	d.Container, d.UseSudo = "gi", true
	if got := restartDesc(d); got != "sudo -n docker restart gi" {
		t.Fatal(got)
	}
	d.Container = "bad name"
	if got := restartDesc(d); got != "docker restart（容器名不合法）" {
		t.Fatal(got) // 非法容器名不会被当成命令写进审计
	}
}

// ---- 真实的本机命令执行（用测试二进制自己当被调用的程序，不依赖系统命令） ----

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GICONFIG_HELPER") == "" {
		return
	}
	switch os.Getenv("GICONFIG_HELPER") {
	case "echo":
		fmt.Println("hello", strings.Join(os.Args[len(os.Args)-2:], "|"))
	case "fail":
		fmt.Fprintln(os.Stderr, "boom")
		os.Exit(3)
	case "sleep":
		time.Sleep(30 * time.Second)
	case "flood":
		fmt.Print(strings.Repeat("x", 100<<10))
	}
	os.Exit(0)
}

func helperArgv(mode string, extra ...string) ([]string, func()) {
	os.Setenv("GICONFIG_HELPER", mode)
	return append([]string{os.Args[0], "-test.run=^TestHelperProcess$", "--"}, extra...), func() { os.Unsetenv("GICONFIG_HELPER") }
}

func TestOSRunner(t *testing.T) {
	argv, done := helperArgv("echo", "a b", "$(reboot)")
	defer done()
	out, err := osRunner{}.Run(bg, argv)
	if err != nil || !strings.Contains(out, "hello a b|$(reboot)") {
		t.Fatalf("参数应原样传递、不经 shell 展开：%q %v", out, err)
	}
	argv, done2 := helperArgv("fail")
	defer done2()
	out, err = osRunner{}.Run(bg, argv)
	if err == nil || !strings.Contains(out, "boom") || !strings.Contains(err.Error(), "命令执行失败") {
		t.Fatalf("%q %v", out, err)
	}
	argv, done3 := helperArgv("flood")
	defer done3()
	out, err = osRunner{}.Run(bg, argv)
	if err != nil || len(out) > maxOutput+100 || !strings.Contains(out, "已截断") {
		t.Fatalf("输出应被截断：len=%d %v", len(out), err)
	}
	if _, err := (osRunner{}).Run(bg, nil); err == nil {
		t.Fatal("空命令应报错")
	}
	if _, err := (osRunner{}).Run(bg, []string{"/definitely/not/here"}); err == nil {
		t.Fatal("不存在的程序应报错")
	}
}

func TestOSRunnerTimeout(t *testing.T) {
	argv, done := helperArgv("sleep")
	defer done()
	ctx, cancel := context.WithTimeout(bg, 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := osRunner{}.Run(ctx, argv)
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("%v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("超时后应尽快返回")
	}
}
