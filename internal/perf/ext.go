// @author: kevinkelly
// @license: Apache Licence
// @file: ext.go
// @time: 2026/10/28

package perf

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"dbtool/internal/settings"
)

// 性能工单的扩展能力：这些数据没有放进基础表（不改基础表结构），而是放在 deploy/sql/060_perf_ext.sql 的旁路表里：
//
//	dbt_perf_issue_ext  工单扩展：标记修复时关联的 SQL 上线工单号
//	dbt_perf_route_ext  路由规则扩展：表名正则
//	dbt_perf_setting    键值设置：SLA 升级通知对象矩阵
//
// 表不存在时功能优雅降级：读取得到默认值（行为与没有这个功能时完全一致），写入返回“请先执行 060_perf_ext.sql”。
// 采集窗口放在 dbt_setting（perf.collect_window_minutes）。

// 旁路表名。
const (
	tblIssueExt = "dbt_perf_issue_ext"
	tblRouteExt = "dbt_perf_route_ext"
	tblSetting  = "dbt_perf_setting"
)

// ErrExtMissing 表示旁路表还没有创建。
var ErrExtMissing = errors.New("perf: 旁路表未创建")

func extMissing(table string) error {
	return conflict(fmt.Sprintf("功能所需的表 %s 还没有创建，请先在元数据库执行 deploy/sql/060_perf_ext.sql", table))
}

// IssueExt 是工单的扩展信息。
type IssueExt struct {
	WorkflowID uint // 标记修复时关联的 SQL 上线工单号，0 表示没有
}

// IssueCounts 是列表页签与 KPI 用的计数（都在当前用户的可见范围内）。
type IssueCounts struct {
	Todo        int `json:"todo"`         // 我的待办
	TodoOverdue int `json:"todo_overdue"` // 其中已超时
	Open        int `json:"open"`         // 全部未关闭
	Overdue     int `json:"overdue"`      // 已超时
	New         int `json:"new"`          // 待分派
	Assigned    int `json:"assigned"`     // 待受理
	Accepted    int `json:"accepted"`     // 处理中
	Fixed       int `json:"fixed"`        // 待验证
	Approve     int `json:"approve"`      // 审批中（豁免审批中，或有待审批的延期申请）
	Closed      int `json:"closed"`
	All         int `json:"all"`
	Created7d   int `json:"created_7d"` // 近 7 天新增
	Closed7d    int `json:"closed_7d"`  // 近 7 天关闭
}

// ExtRepo 是仓库的可选扩展（store.go 实现；没有实现的仓库，对应功能自动降级）。
type ExtRepo interface {
	// ExtReady 判断旁路表是否已创建（tblIssueExt / tblRouteExt / tblSetting）。
	ExtReady(table string) bool
	GetIssueExt(issueID uint) (IssueExt, error)
	SaveIssueExt(issueID uint, e IssueExt) error
	WorkflowExists(id uint) (bool, error) // dbt_sql_workflow（只读）
	ListRouteExt() (map[uint]string, error)
	SaveRouteExt(routeID uint, tblRegex string) error // tblRegex 为空表示删除
	GetSetting(key string) (string, error)            // 不存在返回 "", nil
	SetSetting(key, value, by string) error
	SetConfigValue(item, value, desc string) error // dbt_setting（写入时加密）
	IssueCounts(q IssueQuery) (IssueCounts, error)
}

func (s *Service) ext() ExtRepo {
	e, _ := s.repo.(ExtRepo)
	return e
}

func (s *Service) extReady(table string) bool {
	e := s.ext()
	return e != nil && e.ExtReady(table)
}

// ---- 标记修复关联的上线工单 ----

// checkWorkflow 校验关联的 SQL 上线工单号（0 表示不关联）。
func (s *Service) checkWorkflow(id uint) error {
	if id == 0 {
		return nil
	}
	if !s.extReady(tblIssueExt) {
		return extMissing(tblIssueExt)
	}
	ok, err := s.ext().WorkflowExists(id)
	if err != nil {
		return err
	}
	if !ok {
		return bad(fmt.Sprintf("SQL 上线工单 #%d 不存在", id))
	}
	return nil
}

// saveWorkflowLink 记录（wf=0 表示清除）标记修复时关联的上线工单。
func (s *Service) saveWorkflowLink(issueID, wf uint) {
	if e := s.ext(); e == nil || !e.ExtReady(tblIssueExt) {
		return
	}
	if err := s.ext().SaveIssueExt(issueID, IssueExt{WorkflowID: wf}); err != nil {
		// 工单状态已经保存，关联失败不回滚，只记日志
		logWarn("保存性能工单关联的上线工单失败", issueID, err)
	}
}

func (s *Service) workflowOf(issueID uint) uint {
	e := s.ext()
	if e == nil || !e.ExtReady(tblIssueExt) {
		return 0
	}
	x, err := e.GetIssueExt(issueID)
	if err != nil {
		return 0
	}
	return x.WorkflowID
}

// ---- 修复效果 ----

// FixEffect 是“标记修复”之后的效果：与修复前基线对比（页面的验证弹窗和详情用）。
type FixEffect struct {
	BaselineAvgMs    float64 `json:"baseline_avg_ms"`
	BaselineDailySec float64 `json:"baseline_daily_sec"`
	AfterAvgMs       float64 `json:"after_avg_ms"`
	AfterDailySec    float64 `json:"after_daily_sec"`
	Days             int     `json:"days"`      // 已观察天数（修复当天不计，今天不计）
	NeedDays         int     `json:"need_days"` // 自动验证要求的观察天数
	Absent           bool    `json:"absent"`    // 观察期内没有再出现
	AvgImprovePct    float64 `json:"avg_improve_pct"`
	DailyImprovePct  float64 `json:"daily_improve_pct"`
	NeedPct          float64 `json:"need_pct"` // 自动验证要求的下降比例（%）
	Pass             bool    `json:"pass"`     // 当前数据已满足自动验证条件
	Observed         bool    `json:"observed"` // 是否已有至少一天的观察数据
}

// fixEffect 计算修复效果。没有标记过修复（没有基线）返回 nil。
func (s *Service) fixEffect(i *Issue) *FixEffect {
	if i.FixedAt == nil || (i.BaselineAvgMs <= 0 && i.BaselineDailySec <= 0) {
		return nil
	}
	today := dateOf(s.now())
	needDays := s.cfgInt(settings.KeyPerfVerifyDays, 3)
	ratio := s.cfgFloat(settings.KeyPerfVerifyImproveRatio, 0.5)
	e := &FixEffect{BaselineAvgMs: i.BaselineAvgMs, BaselineDailySec: i.BaselineDailySec, NeedDays: needDays, NeedPct: round(ratio*100, 0)}
	start := dateOf(*i.FixedAt).AddDate(0, 0, 1) // 修复当天数据混杂，不计
	nDays := int(today.Sub(start).Hours()/24 + 0.5)
	if nDays < 1 {
		return e
	}
	until := today
	if i.Status == StClosed && i.ClosedAt != nil && dateOf(*i.ClosedAt).Before(today) {
		until = dateOf(*i.ClosedAt).AddDate(0, 0, 1) // 已关闭的工单只看到关闭那天
		nDays = int(until.Sub(start).Hours()/24 + 0.5)
		if nDays < 1 {
			return e
		}
	}
	agg, err := s.repo.SumStats(i.ID, start, until)
	if err != nil {
		return e
	}
	e.Days, e.Observed = nDays, true
	if agg.ExecCount == 0 {
		e.Absent, e.AvgImprovePct, e.DailyImprovePct, e.Pass = true, 100, 100, true
		return e
	}
	e.AfterAvgMs = round(agg.TotalTime*1000/float64(agg.ExecCount), 1)
	e.AfterDailySec = round(agg.TotalTime/float64(nDays), 1)
	if i.BaselineAvgMs > 0 {
		e.AvgImprovePct = round((1-e.AfterAvgMs/i.BaselineAvgMs)*100, 1)
	}
	if i.BaselineDailySec > 0 {
		e.DailyImprovePct = round((1-e.AfterDailySec/i.BaselineDailySec)*100, 1)
	}
	e.Pass = (i.BaselineAvgMs > 0 && e.AfterAvgMs <= i.BaselineAvgMs*(1-ratio)) || (i.BaselineDailySec > 0 && e.AfterDailySec <= i.BaselineDailySec*(1-ratio))
	return e
}

// ---- 升级通知对象矩阵 ----

// EscTargets 是某一级升级要 @ 的对象。
type EscTargets struct {
	Owner    bool `json:"owner"`    // 负责人
	TL       bool `json:"tl"`       // 团队 TL
	DBA      bool `json:"dba"`      // 跟进 DBA
	Director bool `json:"director"` // 总监
	// NoGlobal 为 true 时这一级的升级消息不发到全局通知渠道（dbt_notify_channel.group_id=0 的渠道），只发团队 / 资源组渠道。
	// 取反存储是为了让零值 = 现状：升级功能上线以来，升级消息一直同时发到全局渠道，没有配置过的矩阵、旧版保存的矩阵都继续如此。
	// 对外的 JSON 字段是正向的 "global"（缺省 = true）。
	NoGlobal bool `json:"-"`
}

type escTargetsJSON struct {
	Owner    bool  `json:"owner"`
	TL       bool  `json:"tl"`
	DBA      bool  `json:"dba"`
	Director bool  `json:"director"`
	Global   *bool `json:"global,omitempty"`
}

func (t EscTargets) MarshalJSON() ([]byte, error) {
	g := !t.NoGlobal
	return json.Marshal(escTargetsJSON{Owner: t.Owner, TL: t.TL, DBA: t.DBA, Director: t.Director, Global: &g})
}

// UnmarshalJSON 兼容没有 global 字段的旧数据 / 旧客户端：缺省 = 发到全局渠道（与升级前的行为一致）。
func (t *EscTargets) UnmarshalJSON(b []byte) error {
	var x escTargetsJSON
	if err := json.Unmarshal(b, &x); err != nil {
		return err
	}
	*t = EscTargets{Owner: x.Owner, TL: x.TL, DBA: x.DBA, Director: x.Director, NoGlobal: x.Global != nil && !*x.Global}
	return nil
}

// EscMatrix 键为 L1 / L2 / L3（L1 催办，L2 升级，L3 升级总监）。
type EscMatrix map[string]EscTargets

const settingEscMatrix = "escalation_matrix"

// defaultEscMatrix 与矩阵功能上线前写死在 notifyEscalation 里的行为完全一致。
func defaultEscMatrix() EscMatrix {
	return EscMatrix{
		"L1": {Owner: true},
		"L2": {Owner: true, TL: true, DBA: true},
		"L3": {Owner: true, TL: true, DBA: true, Director: true},
	}
}

var escLevels = []string{"L1", "L2", "L3"}

// escalationMatrix 返回当前生效的矩阵（读取失败、内容不合法或表不存在都用默认值）。
func (s *Service) escalationMatrix() EscMatrix {
	def := defaultEscMatrix()
	e := s.ext()
	if e == nil || !e.ExtReady(tblSetting) {
		return def
	}
	raw, err := e.GetSetting(settingEscMatrix)
	if err != nil || strings.TrimSpace(raw) == "" {
		return def
	}
	var m EscMatrix
	if json.Unmarshal([]byte(raw), &m) != nil || validateEscMatrix(m) != nil {
		return def
	}
	return m
}

func validateEscMatrix(m EscMatrix) error {
	for _, l := range escLevels {
		t, ok := m[l]
		if !ok {
			return bad(fmt.Sprintf("升级通知对象缺少 %s", l))
		}
		if !t.Owner {
			return bad(fmt.Sprintf("%s：每一级升级都必须通知负责人", l))
		}
	}
	for k := range m {
		if k != "L1" && k != "L2" && k != "L3" {
			return bad(fmt.Sprintf("升级级别不合法：%s", k))
		}
	}
	return nil
}

// targetsFor 返回升级级别（notifyEscalation 的 level：0 即将超时，1~3 对应 L1~L3）要通知的对象。即将超时提醒固定只通知负责人。
func (m EscMatrix) targetsFor(level int) EscTargets {
	if level <= 0 {
		return EscTargets{Owner: true}
	}
	if level > 3 {
		level = 3
	}
	return m[escLevels[level-1]]
}

func (m EscMatrix) equal(o EscMatrix) bool {
	for _, l := range escLevels {
		if m[l] != o[l] {
			return false
		}
	}
	return true
}

func (m EscMatrix) text() string {
	var parts []string
	for _, l := range escLevels {
		t := m[l]
		var who []string
		for _, x := range []struct {
			on   bool
			name string
		}{{t.Owner, "负责人"}, {t.TL, "团队TL"}, {t.DBA, "跟进DBA"}, {t.Director, "总监"}} {
			if x.on {
				who = append(who, x.name)
			}
		}
		s := l + "→" + strings.Join(who, "/")
		if t.NoGlobal {
			s += "（不发全局渠道）"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "；")
}

// SaveEscalation 保存升级通知对象矩阵（与 SLA 时限一起在 SLA 页保存）。
func (s *Service) SaveEscalation(v Viewer, m EscMatrix) error {
	if !v.IsDBA() {
		return forbid("需要 DBA(性能工单管理) 权限")
	}
	if err := validateEscMatrix(m); err != nil {
		return err
	}
	old := s.escalationMatrix()
	if old.equal(m) {
		return nil
	}
	e := s.ext()
	if e == nil || !e.ExtReady(tblSetting) {
		return extMissing(tblSetting)
	}
	b, _ := json.Marshal(m)
	if err := e.SetSetting(settingEscMatrix, string(b), v.Username); err != nil {
		return err
	}
	s.audit(whoOf(v.Actor), "性能工单配置-升级通知对象修改", fmt.Sprintf("旧：%s。新：%s", old.text(), m.text()))
	return nil
}

// ---- 采集窗口 ----

const (
	minWindowMinutes = 5
	maxWindowMinutes = 24 * 60
	cfgCollectWindow = settings.KeyPerfCollectWindowMinutes
)

// WindowInfo 是采集窗口的当前值。
type WindowInfo struct {
	Minutes  int  `json:"minutes"`
	Default  int  `json:"default"`
	Min      int  `json:"min"`
	Max      int  `json:"max"`
	Custom   bool `json:"custom"`   // 是否配置过（dbt_setting 里有值）
	Writable bool `json:"writable"` // 仓库是否支持在页面里修改
}

func (s *Service) windowInfo() WindowInfo {
	w := WindowInfo{Minutes: int(s.collectWindow() / time.Minute), Default: defaultWindowMinutes, Min: minWindowMinutes, Max: maxWindowMinutes, Writable: s.ext() != nil}
	_, w.Custom = s.repo.ConfigValue(cfgCollectWindow)
	return w
}

// SaveCollectWindow 修改采集窗口（分钟）。写入 dbt_setting 的 perf.collect_window_minutes，保存即对下一轮采集生效。
func (s *Service) SaveCollectWindow(v Viewer, minutes int) (WindowInfo, error) {
	if !v.IsDBA() {
		return WindowInfo{}, forbid("需要 DBA(性能工单管理) 权限")
	}
	if minutes < minWindowMinutes || minutes > maxWindowMinutes {
		return WindowInfo{}, bad(fmt.Sprintf("采集窗口需在 %d 到 %d 分钟之间", minWindowMinutes, maxWindowMinutes))
	}
	e := s.ext()
	if e == nil {
		return WindowInfo{}, bad(fmt.Sprintf("当前环境不支持在页面里修改采集窗口，请到“系统管理 → 配置项”修改 %s", cfgCollectWindow))
	}
	old := int(s.collectWindow() / time.Minute)
	if old == minutes {
		if _, ok := s.repo.ConfigValue(cfgCollectWindow); ok {
			return s.windowInfo(), nil
		}
	}
	if err := e.SetConfigValue(cfgCollectWindow, fmt.Sprint(minutes), "性能工单采集窗口（分钟）"); err != nil {
		return WindowInfo{}, err
	}
	s.audit(whoOf(v.Actor), "性能工单配置-采集窗口修改", fmt.Sprintf("%d 分钟 → %d 分钟", old, minutes))
	return s.windowInfo(), nil
}

// ---- 路由规则的表名正则 ----

const maxTblRegexLen = 200

// normTblRegex 规整表名正则：空串与 ".*" 都表示不限制，统一存成空串。
func normTblRegex(in string) (string, error) {
	in = strings.TrimSpace(in)
	if in == "" || in == ".*" {
		return "", nil
	}
	if len([]rune(in)) > maxTblRegexLen {
		return "", bad(fmt.Sprintf("表名正则过长（最多 %d 个字符）", maxTblRegexLen))
	}
	if _, err := regexp.Compile("^(?:" + in + ")$"); err != nil {
		return "", bad(fmt.Sprintf("表名正则不合法：%s", err.Error()))
	}
	return in, nil
}

// tblMatches 路由规则的表名条件：没有设置表名正则时恒为真；设置了时，工单涉及的任一张表整体匹配即可（没有表信息则不匹配）。
func tblMatches(re *regexp.Regexp, tables []string) bool {
	if re == nil {
		return true
	}
	for _, t := range tables {
		if re.MatchString(t) {
			return true
		}
	}
	return false
}

// routeTblRegexes 读取所有路由规则的表名正则（已编译；非法的丢弃并记日志）。
func (s *Service) routeTblRegexes() map[uint]*regexp.Regexp {
	e := s.ext()
	if e == nil || !e.ExtReady(tblRouteExt) {
		return nil
	}
	raw, err := e.ListRouteExt()
	if err != nil || len(raw) == 0 {
		return nil
	}
	out := make(map[uint]*regexp.Regexp, len(raw))
	for id, expr := range raw {
		if strings.TrimSpace(expr) == "" {
			continue
		}
		re, err := regexp.Compile("^(?:" + expr + ")$")
		if err != nil {
			logWarn("性能工单路由规则的表名正则非法，已忽略", id, err)
			continue
		}
		out[id] = re
	}
	return out
}

func logWarn(msg string, id uint, err error) { slog.Warn(msg, "id", id, "err", err) }
