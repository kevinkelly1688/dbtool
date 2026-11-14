// @author: kevinkelly
// @license: Apache Licence
// @file: exec.go
// @time: 2026/10/28

package giconfig

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// 重启方式只有一种：docker restart <容器名>（可选 sudo -n）。容器名来自管理员在“部署与重启”里的设置，
// 在设置时和执行时各校验一次；命令按参数列表直接 exec，不经过 shell。
var containerRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.\-]{0,127}$`)

// ErrNoContainer 表示还没有设置 Docker 容器名，没有可执行的命令。
var ErrNoContainer = errors.New("尚未设置 goInception 的 Docker 容器名")

// BuildArgv 生成要在本机执行的命令（argv）：[sudo -n] docker restart <容器名>。
// 容器名必须匹配 containerRe（首字符为字母或数字，所以不会被 docker 当成选项），不含任何空白、引号或 shell 元字符。
func BuildArgv(d Deploy) ([]string, error) {
	if d.Container == "" {
		return nil, ErrNoContainer
	}
	if !containerRe.MatchString(d.Container) {
		return nil, errors.New("容器名不合法：只允许字母、数字和 _ . -，且以字母或数字开头（最长 128 位）")
	}
	argv := []string{"docker", "restart", d.Container}
	if d.UseSudo {
		argv = append([]string{"sudo", "-n"}, argv...)
	}
	return argv, nil
}

// restartDesc 是写进审计与流程记录里的“重启方式”描述：具体命令；没有设置（或不合法的）容器名时写明原因，不会拼出可疑的命令。
func restartDesc(d Deploy) string {
	argv, err := BuildArgv(d)
	switch {
	case errors.Is(err, ErrNoContainer):
		return "docker restart（未设置容器名）"
	case err != nil:
		return "docker restart（容器名不合法）"
	}
	return strings.Join(argv, " ")
}

// CommandRunner 在本机执行命令（不经 shell）。
type CommandRunner interface {
	Run(ctx context.Context, argv []string) (output string, err error)
}

// Restarter 重启 goInception。
type Restarter interface {
	Restart(ctx context.Context, d Deploy) (output string, err error)
	// Preview 返回将要执行的命令文本（只读预览，不执行任何东西）。
	Preview(d Deploy) (string, error)
}

type execRestarter struct {
	runner CommandRunner
}

func (r execRestarter) Preview(d Deploy) (string, error) {
	argv, err := BuildArgv(d)
	if err != nil {
		return "", err
	}
	return strings.Join(argv, " "), nil
}

func (r execRestarter) Restart(ctx context.Context, d Deploy) (string, error) {
	argv, err := BuildArgv(d)
	if err != nil {
		return "", err
	}
	return r.runner.Run(ctx, argv)
}

// limitedBuffer 只保留前 max 个字节的输出，避免命令刷屏撑爆内存。
type limitedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
			b.truncated = true
		} else {
			b.buf.Write(p)
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := strings.TrimSpace(b.buf.String())
	if b.truncated {
		s += "\n…（输出过长，已截断）"
	}
	return s
}

const maxOutput = 16 << 10

type osRunner struct{}

func (osRunner) Run(ctx context.Context, argv []string) (string, error) {
	if len(argv) == 0 {
		return "", errors.New("命令为空")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out := &limitedBuffer{max: maxOutput}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 3 * time.Second
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return out.String(), errors.New("命令执行超时，已终止")
	}
	if err != nil {
		return out.String(), wrapf(err, "命令执行失败：%s")
	}
	return out.String(), nil
}
