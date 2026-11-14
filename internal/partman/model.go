// @author: kevinkelly
// @license: Apache Licence
// @file: model.go
// @time: 2026/10/28

package partman

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// 配置里的枚举。
const (
	ColInt      = "int"      // 10 位秒级时间戳
	ColBigint   = "bigint"   // 13 位毫秒时间戳
	ColDatetime = "datetime" // YYYY-MM-DD HH:MM:SS
	ColDate     = "date"     // YYYY-MM-DD；RANGE COLUMNS(字段)，与 datetime 同一套做法，周期至少为 day
)

var (
	colTypes = []string{ColInt, ColBigint, ColDatetime, ColDate}
	periods  = []string{"minute", "hour", "day", "week", "month", "year"}
	// timezones 是界面上的“常用项”（接口 overview.timezones）；实际接受的写法见 ParseTimezone。
	timezones = []string{"Asia/Shanghai", "UTC", "Asia/Tokyo", "Asia/Singapore", "America/New_York", "America/Los_Angeles", "Europe/London", "Europe/Berlin", "+08:00"}
)

// Config -> dbt_partman_config（只读写既有列，不改结构）。
// 注意 column / interval 是 MySQL 关键字，gorm 生成的语句会自动加反引号；手写 SQL 时要自己加。
type Config struct {
	ID          uint      `gorm:"column:id;primaryKey"`
	CnfID       int64     `gorm:"column:cnf_id"` // 业务编号，日志按它关联（不是 id）
	InstanceID  uint      `gorm:"column:instance_id"`
	DBName      string    `gorm:"column:dbname"`
	TbName      string    `gorm:"column:tbname"`
	Column      string    `gorm:"column:column"`
	ColType     string    `gorm:"column:col_type"`
	Period      string    `gorm:"column:period"`
	Interval    int       `gorm:"column:interval"`
	Retain      int       `gorm:"column:retain"`
	PreCreate   int       `gorm:"column:preCreate"`
	Timezone    string    `gorm:"column:timezone"`
	IsEnable    bool      `gorm:"column:isenable"`
	CreateTime  time.Time `gorm:"column:create_time"`
	LastestTime time.Time `gorm:"column:lastest_time"` // 库里没有默认值，写入方必须自己设置
	Env         string    `gorm:"column:env"`
}

func (Config) TableName() string { return "dbt_partman_config" }

// Log -> dbt_partman_log（exec_result 是 JSON 文本；外部程序与本包都往里写）。
type Log struct {
	ID         uint      `gorm:"column:id;primaryKey"`
	CnfID      int64     `gorm:"column:cnf_id"`
	ExecResult string    `gorm:"column:exec_result"`
	CreateTime time.Time `gorm:"column:create_time"`
}

func (Log) TableName() string { return "dbt_partman_log" }

// ---- 错误 ----

// Error 是对外可见的业务错误，Message 可直接展示给用户。
type Error struct {
	Status  int
	Code    string
	Message string
	Data    any // 可选：给前端的结构化补充信息（如需要确认的删除列表）
}

func (e *Error) Error() string { return e.Message }

func newErr(status int, code, format string, a ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, a...)}
}

func badReq(format string, a ...any) *Error { return newErr(400, "bad_request", format, a...) }

// ---- 校验 ----

// identRe 是库名、表名、字段名的白名单：只允许 MySQL 无需转义的 ASCII 标识符字符。
// 超出范围的名字（中文表名、带空格或反引号的名字）一律拒绝，换来的是拼接 SQL 时不可能被注入。
var identRe = regexp.MustCompile(`^[0-9A-Za-z_$]{1,64}$`)

// systemDBs 不允许做分区维护。
var systemDBs = map[string]bool{"information_schema": true, "performance_schema": true, "mysql": true, "sys": true}

func validIdent(s string) bool { return identRe.MatchString(s) }

// quote 给已经通过白名单（或来自 information_schema 且通过白名单）的标识符加反引号；仍然转义反引号作为双保险。
func quote(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Location 把配置里的时区名换成 *time.Location（不含“跟随目标库”：空值要读目标库会话时区，见 resolveTimezone）。
func Location(tz string) (*time.Location, error) {
	t, err := ParseTimezone(tz)
	if err != nil {
		return nil, err
	}
	if t.FollowDB {
		return nil, badReq("时区为空表示跟随目标库会话时区，需要连接目标库后才能确定")
	}
	return t.Loc, nil
}

// ConfigInput 是新增 / 编辑配置时前端提交的字段。
type ConfigInput struct {
	InstanceID uint   `json:"instance_id"`
	DBName     string `json:"dbname"`
	TbName     string `json:"tbname"`
	Column     string `json:"column"`
	ColType    string `json:"col_type"`
	Period     string `json:"period"`
	Interval   int    `json:"interval"`
	Retain     int    `json:"retain"`
	PreCreate  int    `json:"pre_create"`
	Timezone   string `json:"timezone"`
	IsEnable   bool   `json:"isenable"`
	Env        string `json:"env"`

	warnings []string // validate 产生的提示（夏令时等），不参与 JSON
}

// subDay 表示周期小于一天（date 字段无法表达）。
func subDay(period string) bool { return period == "minute" || period == "hour" }

const (
	maxInterval  = 255 // 库里是 tinyint unsigned
	maxPreCreate = 100
	maxRetain    = 100000
)

// validate 校验并规整输入（不涉及实例与数据库）。
func (in *ConfigInput) validate() error {
	in.DBName, in.TbName, in.Column = strings.TrimSpace(in.DBName), strings.TrimSpace(in.TbName), strings.TrimSpace(in.Column)
	in.Env = strings.TrimSpace(in.Env)
	if in.InstanceID == 0 {
		return badReq("请选择数据库实例")
	}
	for _, f := range []struct{ label, v string }{{"库名", in.DBName}, {"表名", in.TbName}, {"分区字段", in.Column}} {
		if !validIdent(f.v) {
			return badReq("%s %s 不合法：只允许字母、数字、下划线和 $，最长 64 个字符", f.label, qstr(f.v))
		}
	}
	if systemDBs[strings.ToLower(in.DBName)] {
		return badReq("不允许对系统库 %s 做分区维护", in.DBName)
	}
	if !contains(colTypes, in.ColType) {
		return badReq("字段类型只能是 int、bigint、datetime、date")
	}
	if !contains(periods, in.Period) {
		return badReq("分区周期只能是 minute、hour、day、week、month、year")
	}
	if in.ColType == ColDate && subDay(in.Period) {
		return badReq("date 字段只能精确到天，分区周期不能是 minute 或 hour，请改用 datetime 字段或把周期改为 day 以上")
	}
	if in.Interval < 1 || in.Interval > maxInterval {
		return badReq("周期跨度需要在 1 到 %d 之间", maxInterval)
	}
	if in.Retain < 0 || in.Retain > maxRetain {
		return badReq("保留历史分区数需要在 0（全部保留）到 %d 之间", maxRetain)
	}
	if in.PreCreate < 0 || in.PreCreate > maxPreCreate {
		return badReq("预创建分区数需要在 0 到 %d 之间", maxPreCreate)
	}
	in.Timezone = strings.TrimSpace(in.Timezone)
	tz, err := ParseTimezone(in.Timezone)
	if err != nil {
		return err
	}
	in.warnings = tz.Warnings
	if n := len([]rune(in.Env)); n > 100 {
		return badReq("说明最多 100 个字符")
	}
	return nil
}
