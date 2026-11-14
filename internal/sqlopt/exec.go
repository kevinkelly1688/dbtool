// @author: kevinkelly
// @license: Apache Licence
// @file: exec.go
// @time: 2026/10/28

package sqlopt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// osRunner 用 os/exec 运行外部程序：
//   - 不经 shell，参数逐个传给进程；
//   - 工作目录是一个全新的空临时目录（SOAR 会往当前目录写 soar.log，也避免它把用户给的“路径”当文件读到别处的内容），结束后整个目录删除；
//   - 不继承服务进程的环境变量（里面有数据库连接串、密钥），只给 PATH 等最少的几个；
//   - 有超时，stdout/stderr 各有大小上限（超出部分丢弃但仍排空管道，不会卡住进程）。
type osRunner struct{}

func (osRunner) Run(ctx context.Context, spec ExecSpec) (*ExecResult, error) {
	if spec.Timeout <= 0 {
		spec.Timeout = time.Minute
	}
	if spec.MaxOutput <= 0 {
		spec.MaxOutput = MaxToolOutput
	}
	tmp, err := os.MkdirTemp("", "sqlopt-")
	if err != nil {
		return nil, fmt.Errorf("创建临时目录失败: %w", err)
	}
	defer os.RemoveAll(tmp)

	cctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, spec.Path, spec.Args...)
	cmd.Dir = tmp
	cmd.Env = cleanEnv(tmp)
	cmd.WaitDelay = 2 * time.Second
	out, errb := &limitedBuffer{max: spec.MaxOutput}, &limitedBuffer{max: spec.MaxOutput}
	cmd.Stdout, cmd.Stderr = out, errb

	runErr := cmd.Run()
	res := &ExecResult{
		Stdout:    strings.ToValidUTF8(out.buf.String(), "?"),
		Stderr:    strings.ToValidUTF8(errb.buf.String(), "?"),
		Truncated: out.truncated || errb.truncated,
	}
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		res.ExitCode = -1
		return res, nil
	}
	if ctx.Err() != nil { // 调用方取消
		return nil, ctx.Err()
	}
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		return nil, runErr // 启动失败（找不到、没有执行权限等）
	}
	return res, nil
}

func cleanEnv(tmp string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + tmp, "TMPDIR=" + tmp, "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}
	if runtime.GOOS == "windows" {
		for _, k := range []string{"SystemRoot", "SYSTEMROOT", "windir", "TEMP", "TMP", "PATHEXT"} {
			if v := os.Getenv(k); v != "" {
				env = append(env, k+"="+v)
			}
		}
	}
	return env
}

// limitedBuffer 只保留前 max 字节，后面的丢弃并记下“被截断”。
type limitedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	room := b.max - b.buf.Len()
	if room > 0 {
		n := len(p)
		if n > room {
			n = room
		}
		b.buf.Write(p[:n])
	}
	if len(p) > room {
		b.truncated = true
	}
	return len(p), nil
}

// resolveTool 校验 dbt_setting 里配置的程序路径：必须能找到、不是目录。返回可直接执行的路径。
// 配置里的相对名（如 soar）按 PATH 查找；带目录的路径必须存在且是普通文件。
func resolveTool(label, configured string) (string, error) {
	p := strings.TrimSpace(configured)
	if p == "" {
		return "", newErr(409, "tool_not_configured", fmt.Sprintf("尚未配置%s的程序路径，请联系超级用户在“系统配置”页的“SQL 优化工具”里设置。", label))
	}
	if strings.ContainsAny(p, "\x00\r\n") {
		return "", newErr(409, "tool_not_configured", fmt.Sprintf("%s的程序路径配置不合法。", label))
	}
	if strings.ContainsAny(p, `/\`) {
		st, err := os.Stat(filepath.Clean(p))
		if err != nil || !st.Mode().IsRegular() {
			return "", newErr(409, "tool_missing", fmt.Sprintf("%s的程序不存在或不是可执行文件：%s，请联系管理员检查系统配置。", label, p))
		}
		return filepath.Clean(p), nil
	}
	lp, err := exec.LookPath(p)
	if err != nil {
		return "", newErr(409, "tool_missing", fmt.Sprintf("在 PATH 中找不到%s的程序：%s，请联系管理员检查系统配置。", label, p))
	}
	return lp, nil
}
