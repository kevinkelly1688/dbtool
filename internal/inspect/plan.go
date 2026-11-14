// @author: kevinkelly
// @license: Apache Licence
// @file: plan.go
// @time: 2026/10/28

package inspect

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/robfig/cron/v3"

	"dbtool/internal/i18n"
)

// 周期类型。
const (
	KindDaily   = "daily"
	KindWeekly  = "weekly"
	KindMonthly = "monthly"
	KindCron    = "cron"
)

// 通知方式。
const (
	NotifyNone   = "none"   // 不通知
	NotifyAlert  = "alert"  // 仅出现告警 / 巡检失败时
	NotifyAlways = "always" // 每次计划跑完（汇总）
)

// 计划运行与运行明细的状态。
const (
	RunRunning = "running"
	RunFinish  = "finish"
	RunFailed  = "failed"
	RunSkipped = "skipped"

	ItemOK        = "ok"
	ItemFailed    = "failed"
	ItemSkipped   = "skipped"
	ItemCancelled = "cancelled"
)

const (
	minInterval      = 5 * time.Minute // 两次运行的最小间隔：再密就是在压测生产库
	maxPlanInstances = 500
)

// Plan 是一个定时巡检计划。
type Plan struct {
	ID             uint       `json:"id"`
	Name           string     `json:"name"`
	Enabled        bool       `json:"enabled"`
	Kind           string     `json:"kind"`      // daily / weekly / monthly / cron
	Time           string     `json:"time"`      // HH:MM
	Weekdays       []int      `json:"weekdays"`  // 0=周日 … 6=周六
	MonthDay       int        `json:"month_day"` // 1..28
	Cron           string     `json:"cron"`      // 5 段表达式；Kind 为 cron 时是用户输入，其余是由上面几项生成的
	InstanceIDs    []uint     `json:"instance_ids"`
	TagCodes       []string   `json:"tag_codes"`
	Databases      []string   `json:"databases"` // SQL Server 每库检查项的库范围；空表示全部用户库（最多 200 个）
	Concurrency    int        `json:"concurrency"`
	StaggerSeconds int        `json:"stagger_seconds"`
	SkipBusy       bool       `json:"skip_busy"`
	RetryOnce      bool       `json:"retry_once"`
	TimeoutMinutes int        `json:"timeout_minutes"`
	NotifyMode     string     `json:"notify_mode"`
	NotifyChannels []uint     `json:"notify_channels"`
	RetentionDays  int        `json:"retention_days"`
	NextRunAt      *time.Time `json:"next_run_at"`
	LastRunAt      *time.Time `json:"last_run_at"`
	LastRunID      uint       `json:"last_run_id"`
	CreatedBy      string     `json:"created_by"`
	CreateTime     time.Time  `json:"create_time"`
	SysTime        time.Time  `json:"sys_time"`
}

// Run 是计划的一次运行（定时触发或手动“立即运行”）。
type Run struct {
	ID          uint       `json:"id"`
	PlanID      uint       `json:"plan_id"`
	PlanName    string     `json:"plan_name"`
	Trigger     string     `json:"trigger"` // schedule / manual
	Operator    string     `json:"operator"`
	ScheduledAt time.Time  `json:"scheduled_at"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at"`
	Status      string     `json:"status"`
	Total       int        `json:"total"`
	OK          int        `json:"ok"`
	Failed      int        `json:"failed"`
	Skipped     int        `json:"skipped"`
	Alert       int        `json:"alert"`
	Attention   int        `json:"attention"`
	Note        string     `json:"note"`
	Heartbeat   time.Time  `json:"heartbeat"`
}

// RunItem 是一次运行里某个实例的结果。
type RunItem struct {
	ID           uint   `json:"id"`
	RunID        uint   `json:"run_id"`
	InstanceID   uint   `json:"instance_id"`
	InstanceName string `json:"instance_name"`
	Kind         string `json:"kind"`
	ReportID     uint   `json:"report_id"`
	Status       string `json:"status"`
	Alert        int    `json:"alert"`
	Attention    int    `json:"attention"`
	Message      string `json:"message"`
}

var (
	reHHMM   = regexp.MustCompile(`^([01]\d|2[0-3]):([0-5]\d)$`)
	reDBName = regexp.MustCompile(`^[^\x00-\x1f\[\]]{1,128}$`)
	// 星期名的翻译键（按 0=周日 … 6=周六 排列）；中文就是一个字。
	weekdayName = []string{"日|inspect_weekday", "一|inspect_weekday", "二|inspect_weekday", "三|inspect_weekday", "四|inspect_weekday", "五|inspect_weekday", "六|inspect_weekday"}
)

// CronExpr 返回最终生效的 5 段 cron 表达式。
func (p *Plan) CronExpr() string {
	hm := reHHMM.FindStringSubmatch(p.Time)
	switch p.Kind {
	case KindDaily, KindWeekly, KindMonthly:
		if hm == nil {
			return ""
		}
		h, _ := strconv.Atoi(hm[1])
		m, _ := strconv.Atoi(hm[2])
		switch p.Kind {
		case KindDaily:
			return fmt.Sprintf("%d %d * * *", m, h)
		case KindWeekly:
			days := make([]string, len(p.Weekdays))
			for i, d := range p.Weekdays {
				days[i] = strconv.Itoa(d)
			}
			return fmt.Sprintf("%d %d * * %s", m, h, strings.Join(days, ","))
		default:
			return fmt.Sprintf("%d %d %d * *", m, h, p.MonthDay)
		}
	}
	return strings.Join(strings.Fields(p.Cron), " ")
}

// Describe 是周期的中文说明（通知正文、审计日志用）。
func (p *Plan) Describe() string { return p.DescribeLang(i18n.ZH) }

// DescribeLang 是周期说明的 lang 版本；页面接口按请求语言输出，中文与 Describe 逐字一致。
func (p *Plan) DescribeLang(lang string) string {
	switch p.Kind {
	case KindDaily:
		return i18n.T(lang, "每天 {time}", "time", p.Time)
	case KindWeekly:
		days := make([]string, 0, len(p.Weekdays))
		for _, d := range p.Weekdays {
			if d >= 0 && d <= 6 {
				days = append(days, i18n.T(lang, weekdayName[d]))
			}
		}
		return i18n.T(lang, "每周{days} {time}", "days", strings.Join(days, i18n.T(lang, "、")), "time", p.Time)
	case KindMonthly:
		return i18n.T(lang, "每月 {day} 日 {time}", "day", p.MonthDay, "time", p.Time)
	}
	return "cron " + p.CronExpr()
}

// ParseCron 校验并解析 5 段 cron 表达式。不接受 @every 等描述符与 TZ= 前缀；两次运行间隔必须不小于 5 分钟。
func ParseCron(expr string) (cron.Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron 表达式需要 5 段（分 时 日 月 周），当前是 %d 段", len(fields))
	}
	norm := strings.Join(fields, " ")
	if strings.ContainsAny(norm, "@") || strings.Contains(strings.ToUpper(norm), "TZ=") {
		return nil, fmt.Errorf("cron 表达式不支持 @ 描述符和时区前缀，请写 5 段表达式")
	}
	sched, err := cron.ParseStandard(norm)
	if err != nil {
		return nil, fmt.Errorf("cron 表达式不合法：%v", err)
	}
	// 用一个固定的起点探测最近三次运行，防止过密
	t := time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local)
	a := sched.Next(t)
	if a.IsZero() {
		return nil, fmt.Errorf("cron 表达式没有可运行的时间")
	}
	if b := sched.Next(a); !b.IsZero() && b.Sub(a) < minInterval {
		return nil, fmt.Errorf("两次运行的间隔不能小于 %d 分钟", int(minInterval.Minutes()))
	}
	return sched, nil
}

// NextRun 返回 from 之后的下一次运行时间。
func (p *Plan) NextRun(from time.Time) (time.Time, error) {
	sched, err := ParseCron(p.CronExpr())
	if err != nil {
		return time.Time{}, err
	}
	t := sched.Next(from.In(time.Local))
	if t.IsZero() {
		return time.Time{}, fmt.Errorf("没有可运行的时间")
	}
	return t, nil
}

// NextRuns 返回 from 之后的 n 次运行时间。
func (p *Plan) NextRuns(from time.Time, n int) ([]time.Time, error) {
	sched, err := ParseCron(p.CronExpr())
	if err != nil {
		return nil, err
	}
	var out []time.Time
	t := from.In(time.Local)
	for len(out) < n {
		t = sched.Next(t)
		if t.IsZero() {
			break
		}
		out = append(out, t)
	}
	return out, nil
}

func badInput(format string, a ...any) error {
	return &Error{Status: 400, Code: "bad_request", Message: fmt.Sprintf(format, a...)}
}

// Normalize 校验并规整计划；失败返回带中文说明的 400 错误。
func (p *Plan) Normalize() error {
	p.Name = strings.TrimSpace(p.Name)
	if n := utf8.RuneCountInString(p.Name); n == 0 || n > 60 {
		return badInput("计划名称需要 1 到 60 个字符")
	}
	switch p.Kind {
	case KindDaily, KindWeekly, KindMonthly:
		if !reHHMM.MatchString(p.Time) {
			return badInput("时刻格式应为 HH:MM，例如 02:00")
		}
		if p.Kind == KindWeekly {
			seen := map[int]bool{}
			var days []int
			for _, d := range p.Weekdays {
				if d < 0 || d > 6 {
					return badInput("星期取值应为 0（周日）到 6（周六）")
				}
				if !seen[d] {
					seen[d] = true
					days = append(days, d)
				}
			}
			if len(days) == 0 {
				return badInput("每周巡检至少选择一个星期")
			}
			sort.Ints(days)
			p.Weekdays = days
		}
		if p.Kind == KindMonthly && (p.MonthDay < 1 || p.MonthDay > 28) {
			return badInput("每月的日期只能选 1 到 28 日（避免遇到没有该日期的月份）")
		}
		p.Cron = p.CronExpr()
	case KindCron:
		p.Cron = p.CronExpr()
		p.Time = ""
	default:
		return badInput("周期类型应为 daily / weekly / monthly / cron")
	}
	if _, err := ParseCron(p.Cron); err != nil {
		return badInput("%v", err)
	}
	if p.Concurrency < 1 || p.Concurrency > 10 {
		return badInput("同时巡检的实例数应在 1 到 10 之间")
	}
	if p.StaggerSeconds < 0 || p.StaggerSeconds > 3600 {
		return badInput("错峰间隔应在 0 到 3600 秒之间")
	}
	if p.TimeoutMinutes < 1 || p.TimeoutMinutes > 240 {
		return badInput("单实例超时应在 1 到 240 分钟之间")
	}
	switch p.NotifyMode {
	case "":
		p.NotifyMode = NotifyNone
	case NotifyNone, NotifyAlert, NotifyAlways:
	default:
		return badInput("通知方式不合法")
	}
	if p.NotifyMode != NotifyNone && len(p.NotifyChannels) == 0 {
		return badInput("选择了通知，请至少选一个通知渠道")
	}
	if len(p.NotifyChannels) > 10 {
		return badInput("通知渠道最多选 10 个")
	}
	p.NotifyChannels = uniqUint(p.NotifyChannels)
	if p.RetentionDays < 1 || p.RetentionDays > 3650 {
		return badInput("报告保留天数应在 1 到 3650 之间")
	}
	p.InstanceIDs = uniqUint(p.InstanceIDs)
	var tags []string
	seen := map[string]bool{}
	for _, t := range p.TagCodes {
		if t = strings.TrimSpace(t); t != "" && !seen[t] {
			if len(t) > 50 {
				return badInput("标签代码过长")
			}
			seen[t] = true
			tags = append(tags, t)
		}
	}
	p.TagCodes = tags
	if len(p.InstanceIDs) == 0 && len(p.TagCodes) == 0 {
		return badInput("请至少选择一个巡检实例或一个标签")
	}
	if len(p.InstanceIDs) > maxPlanInstances {
		return badInput("单个计划最多选择 %d 个实例", maxPlanInstances)
	}
	if len(p.Databases) > 50 {
		return badInput("指定的库最多 50 个")
	}
	var dbs []string
	for _, d := range p.Databases {
		if d = strings.TrimSpace(d); d != "" {
			if !reDBName.MatchString(d) {
				return badInput("库名 %q 不合法", d)
			}
			dbs = append(dbs, d)
		}
	}
	p.Databases = dbs
	return nil
}

func uniqUint(in []uint) []uint {
	seen := map[uint]bool{}
	var out []uint
	for _, v := range in {
		if v != 0 && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Error 是带 HTTP 状态的业务错误，Message 可直接展示给用户。
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func errNotFound(what string) error {
	return &Error{Status: 404, Code: "not_found", Message: fmt.Sprintf("%s不存在", what)}
}
func errConflict(msg string) error  { return &Error{Status: 409, Code: "conflict", Message: msg} }
func errForbidden(msg string) error { return &Error{Status: 403, Code: "forbidden", Message: msg} }
