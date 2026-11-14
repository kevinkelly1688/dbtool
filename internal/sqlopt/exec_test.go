// @author: kevinkelly
// @license: Apache Licence
// @file: exec_test.go
// @time: 2026/10/28

package sqlopt

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testExecutable() (string, error) { return os.Executable() }

// TestHelperProcess 不是真正的测试：它被下面的测试当作“外部程序”重新执行。
// 命令行形如：<测试二进制> -test.run=^TestHelperProcess$ -- <模式> [参数…]
func TestHelperProcess(t *testing.T) {
	args := os.Args
	i := 0
	for i < len(args) && args[i] != "--" {
		i++
	}
	if i >= len(args) {
		return // 普通测试运行，什么都不做
	}
	args = args[i+1:]
	switch args[0] {
	case "echo":
		for _, a := range args[1:] {
			fmt.Println("ARG:" + a)
		}
		wd, _ := os.Getwd()
		fmt.Println("CWD:" + wd)
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, "SQLOPT_") {
				fmt.Println("ENV:" + kv)
			}
		}
		// 在临时目录里写个文件，验证结束后目录被整个清理
		_ = os.WriteFile(filepath.Join(wd, "soar.log"), []byte("log"), 0o600)
	case "sleep":
		time.Sleep(30 * time.Second)
	case "flood":
		buf := strings.Repeat("x", 1024)
		for k := 0; k < 300; k++ {
			fmt.Print(buf)
		}
	case "fail":
		fmt.Fprintln(os.Stderr, "boom: 密码 s3cretPW 错误")
		os.Exit(3)
	}
	os.Exit(0)
}

func helperSpec(timeout time.Duration, maxOut int, mode string, args ...string) ExecSpec {
	exe, _ := os.Executable()
	a := append([]string{"-test.run=^TestHelperProcess$", "--", mode}, args...)
	return ExecSpec{Path: exe, Args: a, Timeout: timeout, MaxOutput: maxOut}
}

func TestRunnerNoShellAndCleanEnv(t *testing.T) {
	t.Setenv("SQLOPT_SECRET", "should-not-leak")
	evil := []string{"a b", "; echo pwned", "$(whoami)", "`id`", "x && y", "'quoted'", `"dq"`, "-flag=1", "line1\nline2"}
	res, err := osRunner{}.Run(context.Background(), helperSpec(20*time.Second, 1<<20, "echo", evil...))
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || res.TimedOut {
		t.Fatalf("%+v", res)
	}
	out := strings.ReplaceAll(res.Stdout, "\r\n", "\n")
	for _, a := range evil[:8] {
		if !strings.Contains(out, "ARG:"+a+"\n") {
			t.Errorf("参数 %q 没有原样传入：\n%s", a, out)
		}
	}
	if !strings.Contains(out, "ARG:line1\nline2\n") {
		t.Errorf("含换行的参数应作为单个参数传入：\n%s", out)
	}
	if strings.Contains(out, "ENV:") {
		t.Errorf("不应继承服务进程的环境变量：\n%s", out)
	}
	// 工作目录是临时目录，结束后被清理
	var cwd string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "CWD:") {
			cwd = strings.TrimPrefix(l, "CWD:")
		}
	}
	if cwd == "" || !strings.Contains(filepath.Base(cwd), "sqlopt-") {
		t.Fatalf("工作目录应是 sqlopt- 临时目录：%q", cwd)
	}
	if _, err := os.Stat(cwd); !os.IsNotExist(err) {
		t.Errorf("临时目录应已清理：%v", err)
	}
}

func TestRunnerTimeoutAndLimits(t *testing.T) {
	start := time.Now()
	res, err := osRunner{}.Run(context.Background(), helperSpec(500*time.Millisecond, 1<<20, "sleep"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Fatalf("应超时：%+v", res)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("超时后应尽快返回")
	}

	res, err = osRunner{}.Run(context.Background(), helperSpec(20*time.Second, 1000, "flood"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || len(res.Stdout) != 1000 {
		t.Fatalf("输出应截断到 1000 字节：truncated=%v len=%d", res.Truncated, len(res.Stdout))
	}

	res, err = osRunner{}.Run(context.Background(), helperSpec(20*time.Second, 1<<20, "fail"))
	if err != nil || res.ExitCode != 3 || !strings.Contains(res.Stderr, "boom") {
		t.Fatalf("非零退出码应原样返回：%+v %v", res, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	if _, err := (osRunner{}).Run(ctx, helperSpec(20*time.Second, 1<<20, "sleep")); err == nil {
		t.Fatal("调用方取消应返回错误")
	}

	if _, err := (osRunner{}).Run(context.Background(), ExecSpec{Path: filepath.Join(os.TempDir(), "no-such-tool-xyz"), Timeout: time.Second}); err == nil {
		t.Fatal("程序不存在应返回错误")
	}
}

func TestResolveTool(t *testing.T) {
	if _, err := resolveTool("SOAR", ""); err == nil || codeOf(t, err) != "tool_not_configured" {
		t.Fatalf("空路径应提示未配置：%v", err)
	}
	if _, err := resolveTool("SOAR", "bad\npath"); err == nil {
		t.Fatal("含换行的路径应拒绝")
	}
	if _, err := resolveTool("SOAR", filepath.Join(os.TempDir(), "no-such-tool-xyz")); err == nil || codeOf(t, err) != "tool_missing" {
		t.Fatalf("不存在的路径应提示不存在：%v", err)
	}
	if _, err := resolveTool("SOAR", os.TempDir()); err == nil || codeOf(t, err) != "tool_missing" {
		t.Fatalf("目录不是可执行文件：%v", err)
	}
	if _, err := resolveTool("SOAR", "no-such-tool-xyz-abc"); err == nil || codeOf(t, err) != "tool_missing" {
		t.Fatalf("PATH 里找不到：%v", err)
	}
	if p, err := resolveTool("SOAR", testToolPath); err != nil || p == "" {
		t.Fatalf("存在的程序应通过：%v", err)
	}
}
