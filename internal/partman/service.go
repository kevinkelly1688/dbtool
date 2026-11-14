// @author: kevinkelly
// @license: Apache Licence
// @file: service.go
// @time: 2026/10/28

package partman

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/model"
	"dbtool/internal/query"
)

// 按钮权限码（登记表 internal/btnperm）。
const (
	permMenu   = "partman.view"   // 打开页面、看列表 / 日志 / 预览
	permAdd    = "partman.add"    // 新增配置
	permChange = "partman.edit"   // 编辑配置、启停、告警设置、推送通道
	permDelete = "partman.delete" // 删除配置；执行里包含 DROP PARTITION 时也要求
)

const timeLayout = "2006-01-02 15:04:05"

// Repo 是分区维护需要的元数据库访问（store.go 是 GORM 实现，测试里用内存假实现）。
type Repo interface {
	ListConfigs(f ConfigFilter) ([]Config, int64, error)
	GetConfig(id uint) (*Config, error) // 不存在返回 nil, nil
	FindByKey(instanceID uint, db, table string) (*Config, error)
	InsertConfig(c *Config) error // 分配 ID 与 CnfID（最大值 + 1）
	UpdateConfig(c *Config) error
	DeleteConfig(id uint) error // 连同该配置的日志一起删除
	InsertLog(cnfID int64, result string, at time.Time) (uint, error)
	ListLogs(cnfID int64, limit int) ([]Log, error)
	// ListLogsByCnf 列出这些配置的日志（最新在前，分页），source 为 manual / auto / external 或空（见 logs.go）；返回符合条件的总数。
	ListLogsByCnf(cnfIDs []int64, source string, offset, limit int) ([]Log, int64, error)
	LastLogTimes(cnfIDs []int64) (map[int64]time.Time, error)
	GetLog(id uint) (*Log, error) // 不存在返回 nil, nil
	// 推送通道（dbt_partman_webhook）。onlyEnabled 为 true 时只返回 isenabled=1 的行。
	ListWebhooks(onlyEnabled bool) ([]Webhook, error)
	InsertWebhook(w *Webhook) error
	UpdateWebhook(w *Webhook) error
	DeleteWebhook(id uint) error
	InsertAudit(e *model.AuditEntry) error
	// TryLock 取跨进程的互斥锁（GET_LOCK），拿不到返回 ok=false；release 必须调用。
	TryLock(ctx context.Context, name string) (release func(), ok bool, err error)
}

// ConfigFilter 是配置列表的筛选条件。Limit 为 0 表示不分页（定时维护用）。
type ConfigFilter struct {
	InstanceIDs  []uint // 非空：只看这些实例；AllInstances 为 true 时忽略
	AllInstances bool
	InstanceID   uint
	IsEnable     *bool
	ColType      string
	Period       string
	Search       string
	Offset       int
	Limit        int
}

// Source 是元数据：实例与系统配置。server.Repo 满足它。
type Source interface {
	ListInstances() ([]model.Instance, error)
	EngineConn(inst *model.Instance) engine.Conn
	ConfigValue(item string) (string, bool)
}

// Access 是实例可见范围（query.Authorizer 满足它）：超级用户 / query.admin_run 看全部，其余按资源组。
type Access interface {
	FilterInstances(p query.Principal, list []model.Instance) ([]model.Instance, error)
	CanUseInstance(p query.Principal, inst *model.Instance) error
}

// Caller 是发起操作的用户及其权限判断。
type Caller struct {
	query.Principal
	Can func(code string) bool // 按钮码（partman.add 等）
	// Lang 是请求的界面语言（只用于导出文件等不经 JSON 响应翻译的文案）；空 / zh-CN 按中文处理。
	Lang string
}

// btnRun 是“立即执行维护”的按钮码，与“编辑配置”（partman.edit）分开校验。
const btnRun = "partman.run"

// canRun 判断能否手动执行维护。
func (c Caller) canRun() bool {
	return c.has(btnRun)
}

func (c Caller) has(code string) bool {
	if c.IsSuperuser {
		return true
	}
	return c.Can != nil && c.Can(code)
}

func (c Caller) need(code, what string) error {
	if !c.has(code) {
		return newErr(403, "forbidden", "没有权限%s（需要 %s）", what, code)
	}
	return nil
}

type Service struct {
	repo   Repo
	meta   Source
	access Access
	dial   Dialer
	now    func() time.Time

	mu       sync.Mutex
	inFlight map[int64]bool // 本进程里正在维护的配置（手动与定时互斥）

	// 状态检查（status.go）：缓存与并发闸，同受 mu 保护（statusSem 本身是通道）。
	status    map[uint]statusEntry
	instSems  map[uint]chan struct{}
	statusSem chan struct{}

	pushClient *http.Client // 推送用；为空用默认（10 秒超时、不跟随重定向）。测试里可替换

	alerts AlertRepo // 可用分区告警的存储（Repo 的可选能力，没有实现时整个功能不可用，见 alert.go）
}

func NewService(repo Repo, meta Source, access Access, dial Dialer) *Service {
	if dial == nil {
		dial = DefaultDialer
	}
	s := &Service{repo: repo, meta: meta, access: access, dial: dial, now: time.Now, inFlight: map[int64]bool{},
		status: map[uint]statusEntry{}, instSems: map[uint]chan struct{}{}, statusSem: make(chan struct{}, globalStatusConc)}
	s.alerts, _ = repo.(AlertRepo)
	return s
}

func wrap(err error) error {
	var qe *query.Error
	if errors.As(err, &qe) {
		return &Error{Status: qe.HTTP, Code: qe.Code, Message: qe.Message}
	}
	return err
}

// ---- 实例 ----

func (s *Service) visible(c Caller) ([]model.Instance, error) {
	list, err := s.meta.ListInstances()
	if err != nil {
		return nil, err
	}
	return s.access.FilterInstances(c.Principal, list)
}

func (s *Service) instance(c Caller, id uint) (*model.Instance, error) {
	list, err := s.meta.ListInstances()
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ID == id {
			if err := s.access.CanUseInstance(c.Principal, &list[i]); err != nil {
				return nil, wrap(err)
			}
			return &list[i], nil
		}
	}
	return nil, newErr(404, "not_found", "实例不存在")
}

// dbAllowed 沿用实例的库名过滤：必须匹配 show_db_name_regex（若配置）、且不匹配 denied_db_name_regex。正则从开头匹配。
func dbAllowed(inst *model.Instance, db string) error {
	check := func(expr string) (bool, error) {
		if strings.TrimSpace(expr) == "" {
			return false, nil
		}
		re, err := regexp.Compile(`\A(?:` + expr + `)`)
		if err != nil {
			return false, newErr(500, "bad_config", "实例的库名过滤正则不合法：%s", expr)
		}
		return re.MatchString(db), nil
	}
	if strings.TrimSpace(inst.ShowDBNameRegex) != "" {
		ok, err := check(inst.ShowDBNameRegex)
		if err != nil {
			return err
		}
		if !ok {
			return newErr(403, "db_not_allowed", "库 %s 不在该实例允许展示的库范围内", db)
		}
	}
	if denied, err := check(inst.DeniedDBNameRegex); err != nil {
		return err
	} else if denied {
		return newErr(403, "db_not_allowed", "库 %s 被该实例禁止访问", db)
	}
	return nil
}

// ---- 概览 ----

type InstanceView struct {
	ID        uint   `json:"id"`
	Name      string `json:"name"`
	DBType    string `json:"db_type"`
	Supported bool   `json:"supported"`
	Dialect   string `json:"dialect,omitempty"` // mysql / doris
	CanInit   bool   `json:"can_init"`          // 能把未分区的表初始化为分区表（仅 MySQL 方言）
}

// InitLimits 是初始化为分区表的上限（给界面文案用）。
type InitLimits struct {
	MaxPartitions  int      `json:"max_partitions"`
	WarnPartitions int      `json:"warn_partitions"`
	StartModes     []string `json:"start_modes"`
}

// TimezoneHint 是时区输入框的说明。
type TimezoneHint struct {
	MaxLen     int      `json:"max_len"`
	Empty      string   `json:"empty"`
	Formats    []string `json:"formats"`
	DSTWarning string   `json:"dst_warning"`
}

type Overview struct {
	Instances []InstanceView `json:"instances"`
	ColTypes  []string       `json:"col_types"`
	Periods   []string       `json:"periods"`
	Timezones []string       `json:"timezones"` // 常用项（不含空值）
	CanAdd    bool           `json:"can_add"`
	CanEdit   bool           `json:"can_edit"`
	CanDelete bool           `json:"can_delete"`
	CanRun    bool           `json:"can_run"` // 能立即执行维护（partman.run），与 can_edit 分开
	Auto      AutoSettings   `json:"auto"`

	TimezoneHint TimezoneHint  `json:"timezone_hint"`
	Alert        AlertOverview `json:"alert"`       // 可用分区告警：全局设置与扩展表是否已建
	NotifyOn     string        `json:"notify_on"`   // 全局推送时机 always / on_failure / never
	CanWebhook   bool          `json:"can_webhook"` // 能管理推送通道（change_partmanconfig）
	Init         InitLimits    `json:"init"`
}

func (s *Service) Overview(c Caller) (*Overview, error) {
	list, err := s.visible(c)
	if err != nil {
		return nil, wrap(err)
	}
	ov := &Overview{Instances: []InstanceView{}, ColTypes: colTypes, Periods: periods, Timezones: timezones,
		CanAdd: c.has(permAdd), CanEdit: c.has(permChange), CanDelete: c.has(permDelete), CanRun: c.canRun(), Auto: s.autoSettings(),
		TimezoneHint: TimezoneHint{MaxLen: maxTimezoneLen, Empty: "留空 = 跟随目标库当前会话时区",
			Formats:    []string{"IANA 名字：Asia/Shanghai、America/New_York", "固定偏移：+08:00、+0800、+8、UTC+8、GMT+08:00"},
			DSTWarning: "有夏令时的 IANA 区域建议改用固定偏移"},
		Alert: s.alertOverview(), NotifyOn: s.notifyOn(), CanWebhook: c.has(permChange),
		Init: InitLimits{MaxPartitions: maxInitPartitions, WarnPartitions: warnInitPartitions, StartModes: []string{StartCurrent, StartEarliest, StartDate}}}
	for _, in := range list {
		d := DialectOf(in.DBType)
		ov.Instances = append(ov.Instances, InstanceView{ID: in.ID, Name: in.InstanceName, DBType: in.DBType, Supported: d != "", Dialect: d, CanInit: d == DialectMySQL})
	}
	sort.Slice(ov.Instances, func(i, j int) bool { return ov.Instances[i].Name < ov.Instances[j].Name })
	return ov, nil
}

// ---- 配置 CRUD ----

type ConfigView struct {
	ID           uint   `json:"id"`
	CnfID        int64  `json:"cnf_id"`
	InstanceID   uint   `json:"instance_id"`
	InstanceName string `json:"instance_name"`
	DBType       string `json:"db_type"`
	Supported    bool   `json:"supported"`
	DBName       string `json:"dbname"`
	TbName       string `json:"tbname"`
	Column       string `json:"column"`
	ColType      string `json:"col_type"`
	Period       string `json:"period"`
	Interval     int    `json:"interval"`
	Retain       int    `json:"retain"`
	PreCreate    int    `json:"pre_create"`
	Timezone     string `json:"timezone"`
	IsEnable     bool   `json:"isenable"`
	Env          string `json:"env"`
	CreateTime   string `json:"create_time"`
	LastestTime  string `json:"lastest_time"`
	LastRun      string `json:"last_run"` // 最近一条维护日志的时间（任何来源）
	Dialect      string `json:"dialect,omitempty"`
	CanInit      bool   `json:"can_init"`
	// 可用分区告警：这张表自己的阈值（nil = 跟随全局，0 = 不告警）与仍在静默期内的截止时间（只在 List 里填）。
	AlertThreshold    *int   `json:"alert_threshold"`
	AlertSilenceUntil string `json:"alert_silence_until,omitempty"`
	// Warnings 只在新增 / 编辑的响应里出现（夏令时等提示）。
	Warnings []string `json:"warnings,omitempty"`
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(timeLayout)
}

func view(c Config, inst *model.Instance, last time.Time) ConfigView {
	v := ConfigView{ID: c.ID, CnfID: c.CnfID, InstanceID: c.InstanceID, DBName: c.DBName, TbName: c.TbName, Column: c.Column,
		ColType: c.ColType, Period: c.Period, Interval: c.Interval, Retain: c.Retain, PreCreate: c.PreCreate, Timezone: c.Timezone,
		IsEnable: c.IsEnable, Env: c.Env, CreateTime: fmtTime(c.CreateTime), LastestTime: fmtTime(c.LastestTime), LastRun: fmtTime(last)}
	if inst != nil {
		v.InstanceName, v.DBType, v.Supported = inst.InstanceName, inst.DBType, SupportedDBType(inst.DBType)
		v.Dialect = DialectOf(inst.DBType)
		v.CanInit = v.Dialect == DialectMySQL
	}
	return v
}

// ListQuery 是前端的列表筛选参数。
type ListQuery struct {
	InstanceID uint
	IsEnable   *bool
	ColType    string
	Period     string
	Search     string
	Offset     int
	Limit      int
}

func (s *Service) List(c Caller, q ListQuery) ([]ConfigView, int64, error) {
	insts, err := s.visible(c)
	if err != nil {
		return nil, 0, wrap(err)
	}
	if len(insts) == 0 {
		return []ConfigView{}, 0, nil
	}
	byID := make(map[uint]*model.Instance, len(insts))
	ids := make([]uint, 0, len(insts))
	for i := range insts {
		byID[insts[i].ID] = &insts[i]
		ids = append(ids, insts[i].ID)
	}
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 20
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	rows, total, err := s.repo.ListConfigs(ConfigFilter{InstanceIDs: ids, InstanceID: q.InstanceID, IsEnable: q.IsEnable,
		ColType: q.ColType, Period: q.Period, Search: strings.TrimSpace(q.Search), Offset: q.Offset, Limit: q.Limit})
	if err != nil {
		return nil, 0, err
	}
	cnf := make([]int64, 0, len(rows))
	for _, r := range rows {
		cnf = append(cnf, r.CnfID)
	}
	last, err := s.repo.LastLogTimes(cnf)
	if err != nil {
		return nil, 0, err
	}
	out := make([]ConfigView, 0, len(rows))
	for _, r := range rows {
		out = append(out, view(r, byID[r.InstanceID], last[r.CnfID]))
	}
	s.attachAlerts(rows, out)
	return out, total, nil
}

// checkTarget 校验配置要维护的实例与库：实例可见、类型受支持、库名通过该实例的过滤规则。
func (s *Service) checkTarget(c Caller, in *ConfigInput) (*model.Instance, error) {
	inst, err := s.instance(c, in.InstanceID)
	if err != nil {
		return nil, err
	}
	if !SupportedDBType(inst.DBType) {
		return nil, unsupportedErr(inst.DBType)
	}
	if err := dbAllowed(inst, in.DBName); err != nil {
		return nil, err
	}
	return inst, nil
}

func (s *Service) Create(c Caller, in ConfigInput) (*ConfigView, error) {
	if err := c.need(permAdd, "新增分区配置"); err != nil {
		return nil, err
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	inst, err := s.checkTarget(c, &in)
	if err != nil {
		return nil, err
	}
	if dup, err := s.repo.FindByKey(in.InstanceID, in.DBName, in.TbName); err != nil {
		return nil, err
	} else if dup != nil {
		return nil, newErr(409, "duplicate", "实例 %s 上的 %s.%s 已经有分区配置（编号 %d）", inst.InstanceName, in.DBName, in.TbName, dup.CnfID)
	}
	now := s.now()
	cfg := Config{InstanceID: in.InstanceID, DBName: in.DBName, TbName: in.TbName, Column: in.Column, ColType: in.ColType, Period: in.Period,
		Interval: in.Interval, Retain: in.Retain, PreCreate: in.PreCreate, Timezone: in.Timezone, IsEnable: in.IsEnable, Env: in.Env,
		CreateTime: now, LastestTime: now}
	if err := s.repo.InsertConfig(&cfg); err != nil {
		return nil, err
	}
	s.auditBestEffort(c, "分区维护-新增配置", fmt.Sprintf("配置 #%d：实例 %s，%s.%s，字段 %s(%s)，周期 %s×%d，保留 %d，预创建 %d，时区 %s，启用 %v", // i18n-ignore：审计日志正文，不翻译
		cfg.CnfID, inst.InstanceName, cfg.DBName, cfg.TbName, cfg.Column, cfg.ColType, cfg.Period, cfg.Interval, cfg.Retain, cfg.PreCreate, cfg.Timezone, cfg.IsEnable))
	v := view(cfg, inst, time.Time{})
	v.Warnings = in.warnings
	return &v, nil
}

func (s *Service) load(c Caller, id uint) (*Config, *model.Instance, error) {
	cfg, err := s.repo.GetConfig(id)
	if err != nil {
		return nil, nil, err
	}
	if cfg == nil {
		return nil, nil, newErr(404, "not_found", "分区配置不存在")
	}
	inst, err := s.instance(c, cfg.InstanceID)
	if err != nil {
		return nil, nil, err
	}
	return cfg, inst, nil
}

func (s *Service) Update(c Caller, id uint, in ConfigInput) (*ConfigView, error) {
	if err := c.need(permChange, "编辑分区配置"); err != nil {
		return nil, err
	}
	cfg, _, err := s.load(c, id)
	if err != nil {
		return nil, err
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	inst, err := s.checkTarget(c, &in)
	if err != nil {
		return nil, err
	}
	if dup, err := s.repo.FindByKey(in.InstanceID, in.DBName, in.TbName); err != nil {
		return nil, err
	} else if dup != nil && dup.ID != cfg.ID {
		return nil, newErr(409, "duplicate", "实例 %s 上的 %s.%s 已经有分区配置（编号 %d）", inst.InstanceName, in.DBName, in.TbName, dup.CnfID)
	}
	before := fmt.Sprintf("%s.%s 字段 %s(%s) 周期 %s×%d 保留 %d 预创建 %d 时区 %s 启用 %v", cfg.DBName, cfg.TbName, cfg.Column, cfg.ColType, cfg.Period, cfg.Interval, cfg.Retain, cfg.PreCreate, cfg.Timezone, cfg.IsEnable) // i18n-ignore：审计日志正文
	cfg.InstanceID, cfg.DBName, cfg.TbName, cfg.Column, cfg.ColType, cfg.Period = in.InstanceID, in.DBName, in.TbName, in.Column, in.ColType, in.Period
	cfg.Interval, cfg.Retain, cfg.PreCreate, cfg.Timezone, cfg.IsEnable, cfg.Env = in.Interval, in.Retain, in.PreCreate, in.Timezone, in.IsEnable, in.Env
	cfg.LastestTime = s.now()
	if err := s.repo.UpdateConfig(cfg); err != nil {
		return nil, err
	}
	s.invalidateStatus(cfg.ID)
	s.auditBestEffort(c, "分区维护-编辑配置", fmt.Sprintf("配置 #%d（实例 %s）：%s => %s.%s 字段 %s(%s) 周期 %s×%d 保留 %d 预创建 %d 时区 %s 启用 %v", // i18n-ignore：审计日志正文，不翻译
		cfg.CnfID, inst.InstanceName, before, cfg.DBName, cfg.TbName, cfg.Column, cfg.ColType, cfg.Period, cfg.Interval, cfg.Retain, cfg.PreCreate, cfg.Timezone, cfg.IsEnable))
	v := view(*cfg, inst, time.Time{})
	v.Warnings = in.warnings
	return &v, nil
}

func (s *Service) Delete(c Caller, id uint) error {
	if err := c.need(permDelete, "删除分区配置"); err != nil {
		return err
	}
	cfg, inst, err := s.load(c, id)
	if err != nil {
		return err
	}
	if err := s.repo.DeleteConfig(id); err != nil {
		return err
	}
	if s.alerts != nil { // 告警的阈值 / 状态 / 记录跟着配置走；扩展表没建时忽略
		if err := s.alerts.DeleteAlertData(cfg.CnfID); err != nil && !errors.Is(err, ErrAlertNotReady) {
			slog.Warn("删除分区配置时清理告警数据失败（已忽略）", "cnf_id", cfg.CnfID, "err", err)
		}
	}
	s.invalidateStatus(id)
	s.auditBestEffort(c, "分区维护-删除配置", fmt.Sprintf("配置 #%d（实例 %s）：%s.%s，同时删除了它的维护日志", cfg.CnfID, inst.InstanceName, cfg.DBName, cfg.TbName)) // i18n-ignore：审计日志正文，不翻译
	return nil
}

// ---- 审计 ----

func (s *Service) auditEntry(c Caller, action, extra string) *model.AuditEntry {
	if r := []rune(extra); len(r) > 3000 {
		extra = string(r[:3000]) + "…"
	}
	return &model.AuditEntry{UserID: c.ID, UserName: c.Username, UserDisplay: c.Display, Action: action, ExtraInfo: extra}
}

// auditBestEffort 用于只改元数据的操作：审计写失败只记日志（数据已经改了，回滚不了）。
func (s *Service) auditBestEffort(c Caller, action, extra string) {
	if err := s.repo.InsertAudit(s.auditEntry(c, action, extra)); err != nil {
		slog.Error("写入分区维护审计日志失败", "action", action, "user", c.Username, "err", err)
	}
}

// ---- 日志 ----

type LogItemView struct {
	SQL      string `json:"sql"`
	Status   string `json:"status,omitempty"`
	Error    string `json:"error,omitempty"`
	Operator string `json:"operator,omitempty"`
}

type LogView struct {
	ID          uint          `json:"id"`
	CreateTime  string        `json:"create_time"`
	Items       []LogItemView `json:"items"`
	Raw         string        `json:"raw,omitempty"` // 解析不了时展示原文
	Source      string        `json:"source"`        // manual / auto / external（见 logs.go）
	Operator    string        `json:"operator,omitempty"`
	Status      string        `json:"status"`       // success / partial / failed / unknown（见 logs.go）
	OutputLines int           `json:"output_lines"` // 运行输出的行数；> 0 时可用 GET /logs/:log_id 取明细、/logs/:log_id/output 下载文本
	Kind        string        `json:"kind"`         // maintenance（某配置的一次维护）/ auto_round（定时轮次摘要）/ webhook_test（推送测试）
	Round       *RoundSummary `json:"round,omitempty"`
}

func (s *Service) Logs(c Caller, id uint, limit int) ([]LogView, error) {
	cfg, _, err := s.load(c, id)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := s.repo.ListLogs(cfg.CnfID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]LogView, 0, len(rows))
	for _, r := range rows {
		out = append(out, parseLog(r))
	}
	return out, nil
}

func parseLog(r Log) LogView {
	v := LogView{ID: r.ID, CreateTime: fmtTime(r.CreateTime), Items: []LogItemView{}, Source: SourceExternal, Status: LogUnknown, Kind: "maintenance"}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(r.ExecResult), &arr); err != nil {
		v.Raw = clip(r.ExecResult, 20000)
		return v
	}
	v.Source, v.Operator, v.Status = summarizeLog(arr)
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	for _, m := range arr {
		if str(m, "kind") == traceKind { // 运行输出：不当作语句项展示，只给行数
			if tl, ok := m["trace"].([]any); ok {
				v.OutputLines += len(tl)
			}
			if ev := str(m, "event"); ev != "" {
				v.Kind = ev
			}
			if rm, ok := m["round"]; ok && rm != nil {
				var rs RoundSummary
				if b, err := json.Marshal(rm); err == nil && json.Unmarshal(b, &rs) == nil {
					v.Round = &rs
				}
			}
			continue
		}
		it := LogItemView{SQL: str(m, "sql_text"), Status: str(m, "status"), Error: str(m, "error"), Operator: str(m, "operator")}
		if it.SQL == "" && it.Error == "" {
			continue
		}
		v.Items = append(v.Items, it)
	}
	if len(v.Items) == 0 && v.OutputLines == 0 {
		v.Raw = clip(r.ExecResult, 20000)
	}
	return v
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
