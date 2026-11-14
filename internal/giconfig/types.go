// @author: kevinkelly
// @license: Apache Licence
// @file: types.go
// @time: 2026/10/28

package giconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"dbtool/internal/model"
)

// 版本状态。
const (
	StatusDraft      = "draft"      // 已写入磁盘，还没有重启生效
	StatusApplied    = "applied"    // 已重启并通过健康检查
	StatusFailed     = "failed"     // 重启失败（且没有自动恢复，或恢复也失败了）
	StatusRolledBack = "rolledback" // 重启失败，已自动恢复到上一个生效版本
)

// Error 是业务错误；Status 是 HTTP 状态码，Data 是附带给前端的结构化信息（如校验报告、任务列表）。
type Error struct {
	Status  int
	Code    string
	Message string
	Data    any
}

func (e *Error) Error() string { return e.Message }

func newErr(status int, code, format string, a ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, a...)}
}

func badReq(format string, a ...any) *Error {
	return newErr(http.StatusBadRequest, "bad_request", format, a...)
}
func conflict(code, format string, a ...any) *Error {
	return newErr(http.StatusConflict, code, format, a...)
}

// Version 是配置文件的一个历史版本。Content 是明文（落库时由仓库实现加密）。
type Version struct {
	ID            uint       `json:"id"`
	Version       int        `json:"version"`
	Content       string     `json:"-"`
	ContentSHA256 string     `json:"content_sha256"`
	ChangeSummary string     `json:"change_summary"`
	Operator      string     `json:"operator"`
	Status        string     `json:"status"`
	CreatedAt     time.Time  `json:"created_at"`
	AppliedAt     *time.Time `json:"applied_at,omitempty"`
	Error         string     `json:"error,omitempty"`
}

// ConnInfo 是一组连接信息（goInception 本身，或备份库）。Port 为 0 表示未设置。
type ConnInfo struct {
	Host     string
	Port     int
	User     string
	Password string
}

// Deploy 是“部署与重启”设置。重启方式只有一种：`[sudo -n] docker restart <容器名>`，所以这里没有“方式”字段，
// 只有容器名、是否加 sudo，以及超时、健康检查与回滚策略。
type Deploy struct {
	Container         string `json:"container"` // goInception 的 Docker 容器名；为空表示尚未设置，任何需要重启的动作都会被拒绝
	UseSudo           bool   `json:"use_sudo"`
	RestartTimeoutSec int    `json:"restart_timeout_sec"`
	HealthTimeoutSec  int    `json:"health_timeout_sec"`
	AutoRollback      bool   `json:"auto_rollback"`
	WaitTasks         bool   `json:"wait_tasks"` // 有任务时默认选“等待完成再重启”
	WaitIdleMaxSec    int    `json:"wait_idle_max_sec"`
}

// DefaultDeploy 是尚未配置时的默认值。容器名故意留空：不去猜一个容器名重启，必须由管理员在“部署与重启”里填写。
func DefaultDeploy() Deploy {
	return Deploy{RestartTimeoutSec: 60, HealthTimeoutSec: 60, AutoRollback: true, WaitTasks: true, WaitIdleMaxSec: 600}
}

// legacyDeploy 是旧版本存进 dbt_gi_setting.deploy 的 JSON：除了现在的字段，还有 mode / unit / command / ssh 与 SSH 密文。
// 只用来读取旧数据：已删除的字段一律忽略（SSH 密文也不再解密），保存部署设置时整份 JSON 会被重写，旧字段随之消失。
type legacyDeploy struct {
	Deploy
	Mode string `json:"mode"`
}

// parseDeploy 解析库里的部署设置 JSON，兼容旧版本写入的内容；缺失的字段用默认值补齐。
// 旧的 mode 不是 docker（manual / systemd / command / ssh）时，旧数据里没有一个“曾经用来重启”的容器名，
// 即使 container 字段里残留着值（切换方式前填过），也不沿用：按“尚未设置”处理，避免去重启一个管理员没有明确选用的容器。
func parseDeploy(raw string) (Deploy, error) {
	doc := legacyDeploy{Deploy: DefaultDeploy()}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return DefaultDeploy(), err
	}
	d := doc.Deploy
	d.Container = strings.TrimSpace(d.Container)
	if doc.Mode != "" && doc.Mode != "docker" {
		d.Container, d.UseSudo = "", false
	}
	return d, nil
}

// encodeDeploy 序列化部署设置：只含当前字段。
func encodeDeploy(d Deploy) (string, error) {
	b, err := json.Marshal(d)
	return string(b), err
}

// Repo 是本模块需要的数据访问。
type Repo interface {
	// TableReady 判断 dbt_gi_config_version / dbt_gi_setting 是否已创建（deploy/sql/011_gi_config.sql）。
	TableReady() bool

	// dbt_setting：值经 mirage 解密/加密由实现负责。
	ConfigValue(item string) (string, bool)
	SetConfigValues(items map[string]string) error

	// 版本历史。
	LatestVersion() (*Version, error) // 没有则返回 nil, nil
	LatestByStatus(status string) (*Version, error)
	GetVersion(num int) (*Version, error)
	ListVersions(limit int) ([]Version, error) // 新的在前；不带 Content
	InsertVersion(v *Version) error            // 分配 ID 与版本号
	// UpdateVersionResult 在版本当前状态属于 from 时更新结果，返回是否更新成功。
	UpdateVersionResult(id uint, from []string, status string, appliedAt *time.Time, errMsg string) (bool, error)
	PruneVersions(keep int) error

	// 部署设置与小块状态（最近一次重启结果等）。
	LoadDeploy() (Deploy, bool, error)
	SaveDeploy(d Deploy, operator string) error
	LoadState(key string) (string, error)
	SaveState(key, value string) error

	// 跨进程的“同一时刻只允许一个流程”：ttl 到期自动失效，防止进程崩溃后死锁。
	AcquireLock(owner string, ttl time.Duration) (token string, ok bool, holder string, err error)
	ReleaseLock(token string) error

	InsertAudit(e *model.AuditEntry) error
}

// PingResult 是对 goInception 的连通性探测结果。
type PingResult struct {
	LatencyMS int64  `json:"latency_ms"`
	Version   string `json:"version"`
	Variables int    `json:"variables"`
	Note      string `json:"note,omitempty"`
}

// Task 是 goInception 里一个正在进行的会话/任务。
type Task struct {
	ID      string `json:"id"`
	User    string `json:"user"`
	Host    string `json:"host"`
	DB      string `json:"db"`
	Command string `json:"command"`
	State   string `json:"state"`
	Time    string `json:"time"`
	Info    string `json:"info"`
}

// Tasks 是当前进行中的审核/执行任务。Known=false 表示判断不了（语句不被支持等）。
type Tasks struct {
	Known       bool   `json:"known"`
	Connections int    `json:"connections"`
	Running     int    `json:"running"`
	Items       []Task `json:"items"`
	Note        string `json:"note,omitempty"`
}

// BackupResult 是对备份库的探测结果。
type BackupResult struct {
	LatencyMS int64  `json:"latency_ms"`
	Version   string `json:"version"`
	CanWrite  *bool  `json:"can_write,omitempty"`
	Note      string `json:"note,omitempty"`
}

// Prober 连接 goInception 与备份库。测试里用假实现。
type Prober interface {
	Ping(ctx context.Context, c ConnInfo) (PingResult, error)
	Tasks(ctx context.Context, c ConnInfo) (Tasks, error)
	PingBackup(ctx context.Context, c ConnInfo) (BackupResult, error)
}
