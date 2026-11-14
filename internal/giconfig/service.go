// @author: kevinkelly
// @license: Apache Licence
// @file: service.go
// @time: 2026/10/28

// Package giconfig 实现“平台设置 › 审核引擎”：
//   - 连接信息：读写 dbt_setting 里 go_inception_* 与备份库 inception_remote_backup_*，并提供测试连接；
//   - 配置文件：在线编辑 goInception 的 config.toml（语法与取值校验、版本历史、对比、回滚）；
//   - 重启：只支持 docker（`[sudo -n] docker restart <容器名>`），写文件 → 重启 → 健康检查，
//     失败自动恢复上一个生效版本。
package giconfig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// dbt_setting 里的配置项。
const (
	cfgHost    = settings.KeyGoInceptionHost
	cfgPort    = settings.KeyGoInceptionPort
	cfgUser    = settings.KeyGoInceptionUser
	cfgPass    = settings.KeyGoInceptionPassword
	cfgBkHost  = settings.KeyGoInceptionBackupHost
	cfgBkPort  = settings.KeyGoInceptionBackupPort
	cfgBkUser  = settings.KeyGoInceptionBackupUser
	cfgBkPass  = settings.KeyGoInceptionBackupPassword
	cfgFile    = settings.KeyGoInceptionConfigFile // 本模块新增：配置文件路径（环境变量 GOINCEPTION_CONFIG_FILE 优先）
	envFileVar = "GOINCEPTION_CONFIG_FILE"
)

// 审计动作名。
const (
	actConn     = "修改goInception连接信息" // i18n-ignore 审计文本 / 存库的版本摘要，不翻译
	actDeploy   = "修改goInception部署设置" // i18n-ignore 审计文本 / 存库的版本摘要，不翻译
	actSave     = "保存goInception配置"   // i18n-ignore 审计文本 / 存库的版本摘要，不翻译
	actRestart  = "重启goInception"     // i18n-ignore 审计文本 / 存库的版本摘要，不翻译
	actRollback = "回滚goInception配置"   // i18n-ignore 审计文本 / 存库的版本摘要，不翻译
	actImport   = "导入goInception配置文件" // i18n-ignore 审计文本 / 存库的版本摘要，不翻译
)

const keepVersions = 50 // 历史版本保留个数

// Options 允许替换外部依赖（测试用）。零值字段使用真实实现。
type Options struct {
	Files        FileStore
	Prober       Prober
	Runner       CommandRunner // 执行重启命令（docker restart）；测试里用假的，只记录 argv
	Env          func(string) string
	Now          func() time.Time
	Sleep        func(ctx context.Context, d time.Duration) error
	PollInterval time.Duration // 健康检查与等待任务的轮询间隔，默认 2 秒
}

// Service 是 goInception 配置服务。
type Service struct {
	repo      Repo
	files     FileStore
	prober    Prober
	restarter Restarter
	env       func(string) string
	now       func() time.Time
	sleep     func(ctx context.Context, d time.Duration) error
	poll      time.Duration

	opMu sync.Mutex // 进程内“同一时刻只有一个保存/重启流程”；跨进程靠 Repo.AcquireLock
	mu   sync.Mutex // 保护 job
	job  *Job
	wg   sync.WaitGroup
}

func NewService(repo Repo, o Options) *Service {
	s := &Service{repo: repo, files: o.Files, prober: o.Prober, env: o.Env, now: o.Now, sleep: o.Sleep, poll: o.PollInterval}
	if s.files == nil {
		s.files = osFiles{}
	}
	if s.prober == nil {
		s.prober = sqlProber{}
	}
	if o.Runner == nil {
		o.Runner = osRunner{}
	}
	s.restarter = execRestarter{runner: o.Runner}
	if s.env == nil {
		s.env = os.Getenv
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.sleep == nil {
		s.sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	if s.poll <= 0 {
		s.poll = 2 * time.Second
	}
	return s
}

// Wait 等待后台的重启流程结束（测试与优雅退出用）。
func (s *Service) Wait() { s.wg.Wait() }

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (s *Service) audit(actor audit.Actor, action, format string, a ...any) {
	e := &model.AuditEntry{UserID: actor.ID, UserName: actor.Username, UserDisplay: actor.Display, Action: action, ExtraInfo: fmt.Sprintf(format, a...)}
	_ = s.repo.InsertAudit(e)
}

func (s *Service) needTables() error {
	if !s.repo.TableReady() {
		return conflict("table_missing", "goInception 配置相关的表还没有创建，请先在数据库里执行 deploy/sql/011_gi_config.sql")
	}
	return nil
}

// ---- 配置文件路径 ----

// configPath 返回配置文件路径及来源：env（环境变量，页面上不可改）、config（dbt_setting）、none。
func (s *Service) configPath() (string, string, error) {
	if v := strings.TrimSpace(s.env(envFileVar)); v != "" {
		p, err := validConfigPath(v)
		if err != nil {
			return "", "env", wrapf(err, "环境变量 %s 不合法：%s", envFileVar)
		}
		return p, "env", nil
	}
	if v, ok := s.repo.ConfigValue(cfgFile); ok {
		p, err := validConfigPath(v)
		if err != nil {
			return "", "config", wrapf(err, "配置项 %s 不合法：%s", cfgFile)
		}
		return p, "config", nil
	}
	return "", "none", nil
}

func (s *Service) needPath() (string, error) {
	p, _, err := s.configPath()
	if err != nil {
		return "", badReq("%s", err.Error())
	}
	if p == "" {
		return "", newErr(400, "no_path", "还没有配置 goInception 配置文件的路径，请先在“部署与重启”里填写（或设置环境变量 %s）", envFileVar)
	}
	return p, nil
}

// ---- 连接信息 ----

var hostRe = regexp.MustCompile(`^[A-Za-z0-9._\-:]{1,255}$`)

func (s *Service) loadConn(target string) ConnInfo {
	keys := [4]string{cfgHost, cfgPort, cfgUser, cfgPass}
	if target == "backup" {
		keys = [4]string{cfgBkHost, cfgBkPort, cfgBkUser, cfgBkPass}
	}
	var c ConnInfo
	c.Host, _ = s.repo.ConfigValue(keys[0])
	if v, ok := s.repo.ConfigValue(keys[1]); ok {
		c.Port, _ = strconv.Atoi(strings.TrimSpace(v))
	}
	c.User, _ = s.repo.ConfigValue(keys[2])
	c.Password, _ = s.repo.ConfigValue(keys[3])
	c.Host = strings.TrimSpace(c.Host)
	return c
}

// ConnView 是连接信息的展示形式：密码不回显。
type ConnView struct {
	Host        string `json:"host"`
	Port        string `json:"port"`
	User        string `json:"user"`
	HasPassword bool   `json:"has_password"`
}

func connView(c ConnInfo) ConnView {
	v := ConnView{Host: c.Host, User: c.User, HasPassword: c.Password != ""}
	if c.Port != 0 {
		v.Port = strconv.Itoa(c.Port)
	}
	return v
}

// ConnInput 是页面提交的连接信息。Password 留空表示不修改（ClearPassword 为真则清空）。
type ConnInput struct {
	Target        string `json:"target"` // main / backup
	Host          string `json:"host"`
	Port          string `json:"port"`
	User          string `json:"user"`
	Password      string `json:"password"`
	ClearPassword bool   `json:"clear_password"`
}

func normTarget(t string) (string, error) {
	switch t {
	case "", "main":
		return "main", nil
	case "backup":
		return "backup", nil
	}
	return "", badReq("未知的连接类型")
}

func (in *ConnInput) validate() (port int, err error) {
	in.Host, in.Port, in.User = strings.TrimSpace(in.Host), strings.TrimSpace(in.Port), strings.TrimSpace(in.User)
	if in.Host != "" && !hostRe.MatchString(in.Host) {
		return 0, badReq("地址只能包含字母、数字和 . _ - :（最长 255 个字符）")
	}
	if in.Port != "" {
		port, err = strconv.Atoi(in.Port)
		if err != nil || port < 1 || port > 65535 {
			return 0, badReq("端口需要是 1 到 65535 之间的整数")
		}
	}
	if len(in.User) > 100 || strings.ContainsAny(in.User, "\r\n\x00") {
		return 0, badReq("账号过长或含有非法字符")
	}
	if len(in.Password) > 200 || strings.ContainsAny(in.Password, "\r\n\x00") {
		return 0, badReq("密码过长（最多 200 个字符）或含有换行")
	}
	return port, nil
}

// SaveConn 保存连接信息到 dbt_setting，保存即生效（审核/查询每次都读 dbt_setting）。
func (s *Service) SaveConn(actor audit.Actor, in ConnInput) error {
	target, err := normTarget(in.Target)
	if err != nil {
		return err
	}
	if _, err := in.validate(); err != nil {
		return err
	}
	keys := [4]string{cfgHost, cfgPort, cfgUser, cfgPass}
	label := "goInception"
	if target == "backup" {
		keys = [4]string{cfgBkHost, cfgBkPort, cfgBkUser, cfgBkPass}
		label = "备份库" // i18n-ignore 审计文本 / 存库的版本摘要，不翻译
	}
	items := map[string]string{keys[0]: in.Host, keys[1]: in.Port, keys[2]: in.User}
	pwdNote := "密码未修改" // i18n-ignore 审计文本 / 存库的版本摘要，不翻译
	switch {
	case in.ClearPassword:
		items[keys[3]] = ""
		pwdNote = "密码已清空" // i18n-ignore 审计文本 / 存库的版本摘要，不翻译
	case in.Password != "":
		items[keys[3]] = in.Password
		pwdNote = "密码已修改" // i18n-ignore 审计文本 / 存库的版本摘要，不翻译
	}
	if err := s.repo.SetConfigValues(items); err != nil {
		return newErr(500, "save_failed", "保存失败：%v", err)
	}
	s.audit(actor, actConn, "%s：%s:%s，账号 %s，%s", label, in.Host, in.Port, in.User, pwdNote) // i18n-ignore 审计文本 / 存库的版本摘要，不翻译
	return nil
}

// TestResult 是测试连接的结果。
type TestResult struct {
	OK    bool     `json:"ok"`
	Lines []string `json:"lines"`
}

// TestConn 测试连接。表单里的地址/账号优先；密码留空时仅当地址、端口、账号都没变才沿用已保存的密码
// （避免把已保存的密码发给用户新填的地址）。
func (s *Service) TestConn(ctx context.Context, in ConnInput) (*TestResult, error) {
	target, err := normTarget(in.Target)
	if err != nil {
		return nil, err
	}
	port, err := in.validate()
	if err != nil {
		return nil, err
	}
	if in.Host == "" {
		return nil, badReq("请先填写地址")
	}
	stored := s.loadConn(target)
	c := ConnInfo{Host: in.Host, Port: port, User: in.User, Password: in.Password}
	if in.Password == "" && !in.ClearPassword {
		if stored.Host == c.Host && stored.Port == c.Port && stored.User == c.User {
			c.Password = stored.Password
		} else if stored.Password != "" {
			return &TestResult{OK: false, Lines: []string{"修改了地址、端口或账号，请重新输入密码后再测试（不会把已保存的密码发给新的地址）"}}, nil
		}
	}
	addr := c.Host
	if c.Port != 0 {
		addr += ":" + strconv.Itoa(c.Port)
	}
	if target == "backup" {
		r, err := s.prober.PingBackup(ctx, c)
		if err != nil {
			return &TestResult{Lines: []string{fmt.Sprintf("无法连接备份库 %s：%v", addr, err)}}, nil
		}
		res := &TestResult{OK: true, Lines: []string{fmt.Sprintf("连接备份库 %s 成功（延迟 %d ms）", addr, r.LatencyMS)}}
		if r.Version != "" {
			res.Lines = append(res.Lines, fmt.Sprintf("MySQL 版本 %s", r.Version))
		}
		if r.CanWrite != nil && *r.CanWrite {
			res.Lines = append(res.Lines, "账号有写权限")
		}
		if r.Note != "" {
			res.Lines = append(res.Lines, fmt.Sprintf("注意：%s", r.Note))
		}
		return res, nil
	}
	r, err := s.prober.Ping(ctx, c)
	if err != nil {
		return &TestResult{Lines: []string{fmt.Sprintf("无法连接 goInception %s：%v", addr, err)}}, nil
	}
	res := &TestResult{OK: true, Lines: []string{fmt.Sprintf("连接 goInception %s 成功（延迟 %d ms）", addr, r.LatencyMS)}}
	if r.Version != "" {
		res.Lines = append(res.Lines, fmt.Sprintf("goInception 版本 %s", r.Version))
	}
	if r.Variables > 0 {
		res.Lines = append(res.Lines, fmt.Sprintf("`inception get variables` 返回 %d 项，确认对方是 goInception", r.Variables))
	}
	if r.Note != "" {
		res.Lines = append(res.Lines, fmt.Sprintf("注意：%s", r.Note))
	}
	return res, nil
}

// ---- 部署设置 ----

func (s *Service) loadDeploy() (Deploy, error) {
	if !s.repo.TableReady() {
		return DefaultDeploy(), nil // 表还没创建：用默认值，页面会提示去建表
	}
	d, ok, err := s.repo.LoadDeploy()
	if err != nil {
		return Deploy{}, err
	}
	if !ok {
		return DefaultDeploy(), nil
	}
	return d, nil
}

// needRestartable 在任何需要重启的动作（保存并重启、仅重启、回滚）开始前调用：容器名没有设置或不合法时直接拒绝，
// 此时还没有加锁、没有写版本记录、没有改文件，更不会执行任何命令。“仅保存（不重启）”不受影响。
func needRestartable(d Deploy) error {
	_, err := BuildArgv(d)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNoContainer):
		return conflict("deploy_not_set", "尚未设置 goInception 的 Docker 容器名，请先到「部署与重启」里填写")
	}
	return conflict("deploy_invalid", "已保存的 Docker 容器名不合法（%v），请先到「部署与重启」里修正", err)
}

// DeployInput 是页面提交的部署设置。旧版页面多提交的 mode / unit / command / ssh 等字段会被直接忽略。
type DeployInput struct {
	Deploy
	ConfigPath string `json:"config_path"`
}

// DeployView 是部署设置的展示形式。
type DeployView struct {
	Deploy
	ConfigPath       string `json:"config_path"`
	ConfigPathSource string `json:"config_path_source"` // env / config / none
	Configured       bool   `json:"configured"`         // 容器名已设置且合法，可以用页面重启
	Preview          string `json:"preview"`            // 将执行的命令（只读预览）
	PreviewError     string `json:"preview_error,omitempty"`
}

func (s *Service) deployView(d Deploy) DeployView {
	v := DeployView{Deploy: d}
	v.ConfigPath, v.ConfigPathSource, _ = s.configPath()
	if v.ConfigPathSource == "config" || v.ConfigPathSource == "none" {
		if raw, ok := s.repo.ConfigValue(cfgFile); ok {
			v.ConfigPath = raw
		}
	}
	if p, err := s.restarter.Preview(d); err == nil {
		v.Preview, v.Configured = p, true
	} else if !errors.Is(err, ErrNoContainer) { // 没设置容器名不是“错误”，由 Configured=false 表示
		v.PreviewError = err.Error()
	}
	return v
}

// PreviewDeploy 返回按当前表单内容将执行的重启命令（只读预览，不执行）；容器名没有设置或不合法时返回说明。
func (s *Service) PreviewDeploy(d Deploy) (preview, problem string) {
	d.Container = strings.TrimSpace(d.Container)
	p, err := s.restarter.Preview(d)
	if err != nil {
		return "", err.Error()
	}
	return p, ""
}

func validateDeploy(d *Deploy) error {
	if d.RestartTimeoutSec < 5 || d.RestartTimeoutSec > 600 {
		return badReq("重启命令的超时时间需要在 5 到 600 秒之间")
	}
	if d.HealthTimeoutSec < 5 || d.HealthTimeoutSec > 600 {
		return badReq("健康检查的最长等待需要在 5 到 600 秒之间")
	}
	if d.WaitIdleMaxSec < 0 || d.WaitIdleMaxSec > 3600 {
		return badReq("等待任务完成的最长时间需要在 0 到 3600 秒之间")
	}
	d.Container = strings.TrimSpace(d.Container)
	if d.Container != "" { // 留空是允许保存的：表示尚未设置，重启类动作会被拒绝
		if _, err := BuildArgv(*d); err != nil {
			return badReq("%s", err.Error())
		}
	}
	return nil
}

// SaveDeploy 保存部署设置。库里的 JSON 整份重写，旧版本留下的 mode / unit / command / ssh 与 SSH 密文随之消失。
func (s *Service) SaveDeploy(actor audit.Actor, in DeployInput) (*DeployView, error) {
	if err := s.needTables(); err != nil {
		return nil, err
	}
	d := in.Deploy
	if err := validateDeploy(&d); err != nil {
		return nil, err
	}
	var err error
	if _, src, _ := s.configPath(); src != "env" {
		p := strings.TrimSpace(in.ConfigPath)
		if p != "" {
			if p, err = validConfigPath(p); err != nil {
				return nil, badReq("%s", err.Error())
			}
		}
		if err := s.repo.SetConfigValues(map[string]string{cfgFile: p}); err != nil {
			return nil, newErr(500, "save_failed", "保存失败：%v", err)
		}
	}
	if err := s.repo.SaveDeploy(d, actor.Username); err != nil {
		return nil, newErr(500, "save_failed", "保存失败：%v", err)
	}
	s.audit(actor, actDeploy, "重启方式：%s；配置文件：%s；健康检查等待 %d 秒；失败自动回滚：%v", // i18n-ignore 审计文本 / 存库的版本摘要，不翻译
		restartDesc(d), strings.TrimSpace(in.ConfigPath), d.HealthTimeoutSec, d.AutoRollback)
	v := s.deployView(d)
	return &v, nil
}

// CheckResult 是“检查权限与连通性”的结果。
type CheckResult struct {
	OK    bool     `json:"ok"`
	Lines []string `json:"lines"`
}

// CheckDeploy 检查配置文件读写权限、重启命令是否可生成、goInception 是否可连接。只读：不会执行 docker restart，也不会碰容器。
func (s *Service) CheckDeploy(ctx context.Context) (*CheckResult, error) {
	d, err := s.loadDeploy()
	if err != nil {
		return nil, newErr(500, "load_failed", "读取部署设置失败：%v", err)
	}
	res := &CheckResult{OK: true}
	fail := func(format string, a ...any) {
		res.OK = false
		res.Lines = append(res.Lines, "✘ "+fmt.Sprintf(format, a...))
	}
	okf := func(format string, a ...any) { res.Lines = append(res.Lines, "✔ "+fmt.Sprintf(format, a...)) }

	path, _, perr := s.configPath()
	switch {
	case perr != nil:
		fail("%v", perr)
	case path == "":
		fail("还没有配置配置文件路径")
	default:
		if err := s.files.CheckWritable(path); err != nil {
			fail("%v", err)
		} else {
			okf("配置文件 %s 可读写，可以原子替换", path)
		}
	}
	switch p, err := s.restarter.Preview(d); {
	case errors.Is(err, ErrNoContainer):
		fail("尚未设置 goInception 的 Docker 容器名：保存并重启、仅重启和回滚都不可用，请先在上面填写")
	case err != nil:
		fail("重启命令不合法：%v", err)
	default:
		okf("将执行：%s", p)
	}
	if c := s.loadConn("main"); c.Host == "" {
		fail("还没有配置 goInception 的连接信息，重启后无法做健康检查")
	} else if r, err := s.prober.Ping(ctx, c); err != nil {
		res.Lines = append(res.Lines, "ℹ "+fmt.Sprintf("当前连不上 goInception（%s）；如果它此刻没有运行，重启后的健康检查才有意义", err.Error()))
	} else {
		okf("goInception 当前可连接（延迟 %d ms）", r.LatencyMS)
	}
	return res, nil
}
