// @author: kevinkelly
// @license: Apache Licence
// @file: tools.go
// @time: 2026/10/28

package sqlopt

import (
	"context"
	"fmt"
	"strings"
	"time"

	"dbtool/internal/engine"
)

// 外部工具的调用约定：
//   - 程序路径来自 dbt_setting（soar / sqladvisor / optimizer.soar_test_dsn；另支持可选的 optimizer.soar_config 指定 SOAR 配置文件）；
//   - 参数由服务端按固定模板拼装，用户输入（SQL、库名）只作为单个参数传入，不经 shell；
//   - 目标实例的账号密码只出现在命令行参数里（SOAR / SQLAdvisor 本身不支持别的传法），
//     工具输出里出现的密码会被替换成 ******。

func (s *Service) cfgValue(keys ...string) string {
	for _, k := range keys {
		if v, ok := s.repo.ConfigValue(k); ok {
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
	}
	return ""
}

type soarConf struct {
	Path    string
	Config  string // 可选：-config 配置文件
	TestDSN string // 测试环境（SOAR 用来做 EXPLAIN、建临时表），不能是线上库
}

// soarConfig 读取 SOAR 配置并确认程序存在。
func (s *Service) soarConfig() (soarConf, error) {
	p, err := resolveTool("SOAR", s.cfgValue(configSoar))
	if err != nil {
		return soarConf{}, err
	}
	return soarConf{Path: p, Config: s.cfgValue(configSoarConfig), TestDSN: s.cfgValue(configSoarTestDSN)}, nil
}

func dsnOf(c engine.Conn, db string) string {
	return fmt.Sprintf("%s:%s@%s:%d/%s", c.User, c.Password, c.Host, c.Port, db)
}

// soarArgs 是 SOAR 的固定参数模板。-allow-online-as-test=false：线上实例不会被当成测试环境（SOAR 会在测试环境里建表做验证）。
func soarArgs(cf soarConf, online, test, query string) []string {
	var args []string
	if cf.Config != "" {
		args = append(args, "-config", cf.Config)
	}
	args = append(args, "-report-type", "markdown")
	if online != "" {
		args = append(args, "-online-dsn", online)
	}
	if test != "" {
		args = append(args, "-test-dsn", test)
	}
	return append(args, "-allow-online-as-test=false", "-query", query)
}

// runExternal 运行外部程序并整理输出。mergeStderr 为 true 时把 stderr 接在 stdout 后面。
func (s *Service) runExternal(ctx context.Context, label, path string, args []string, timeout time.Duration, maxOut int, mergeStderr bool, secrets ...string) (out string, truncated bool, err error) {
	res, err := s.runner.Run(ctx, ExecSpec{Path: path, Args: args, Timeout: timeout, MaxOutput: maxOut})
	if err != nil {
		if ctx.Err() != nil {
			return "", false, newErr(408, "canceled", fmt.Sprintf("%s 已取消", label))
		}
		return "", false, newErr(502, "tool_failed", fmt.Sprintf("启动 %s 失败：%s", label, redact(err.Error(), secrets...)))
	}
	if res.TimedOut {
		return "", false, newErr(504, "timeout", fmt.Sprintf("%s 运行超过 %d 秒，已终止", label, int(timeout.Seconds())))
	}
	stdout, stderr := res.Stdout, res.Stderr
	if res.ExitCode != 0 && strings.TrimSpace(stdout) == "" {
		return "", false, newErr(502, "tool_failed", fmt.Sprintf("%s 运行失败（退出码 %d）：%s", label, res.ExitCode, truncRunes(redact(strings.TrimSpace(stderr), secrets...), 800)))
	}
	switch {
	case mergeStderr && strings.TrimSpace(stderr) != "":
		out = stdout + stderr
	case strings.TrimSpace(stdout) != "":
		out = stdout
	default:
		out = stderr
	}
	out = redact(out, secrets...)
	truncated = res.Truncated
	if len(out) > maxOut {
		out, truncated = out[:maxOut], true
		out = strings.ToValidUTF8(out, "")
	}
	return out, truncated, nil
}

// OptimizeResult 是“优化工具”一次运行的结果。
type OptimizeResult struct {
	Tool      string        `json:"tool"`
	Output    string        `json:"output,omitempty"` // SOAR Markdown 报告
	Truncated bool          `json:"truncated,omitempty"`
	Tuning    *TuningResult `json:"tuning,omitempty"`
}

// runSoar 运行 SOAR 并取 Markdown 报告。online 为空时不连线上库（只做离线分析）。
func (s *Service) runSoar(ctx context.Context, cf soarConf, conn engine.Conn, db string, stmt string, maxOut int, withOnline bool) (string, bool, error) {
	online, test := "", ""
	if withOnline {
		online, test = dsnOf(conn, db), cf.TestDSN
	}
	return s.runExternal(ctx, "SOAR", cf.Path, soarArgs(cf, online, test, stmt), soarTimeout, maxOut, false, conn.Password, cf.TestDSN, dsnPassword(cf.TestDSN))
}

// toolDB 校验传给外部工具的库名（白名单字符）。
func toolDB(db string) error {
	if db != "" && !toolDBNameRe.MatchString(db) {
		return bad("库名含有不支持的字符：传给 SOAR 的库名只允许字母、数字、下划线、$、点和减号")
	}
	return nil
}

// dsnPassword 取 user:password@host:port/db 形式 DSN 里的密码部分（用于遮盖输出）。
func dsnPassword(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		return ""
	}
	cred := dsn[:at]
	if i := strings.Index(cred, ":"); i >= 0 {
		return cred[i+1:]
	}
	return ""
}
