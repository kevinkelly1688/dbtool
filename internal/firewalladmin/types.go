// @author: kevinkelly
// @license: Apache Licence
// @file: types.go
// @time: 2026/10/28

package firewalladmin

import (
	"dbtool/internal/firewall"
)

// Error 是带 HTTP 状态与错误码的业务错误。
type Error struct {
	Status  int
	Code    string
	Message string
	Data    any
}

func (e *Error) Error() string { return e.Message }

func badReq(msg string) *Error   { return &Error{Status: 400, Code: "bad_request", Message: msg} }
func notFound(msg string) *Error { return &Error{Status: 404, Code: "not_found", Message: msg} }
func conflict(code, msg string) *Error {
	return &Error{Status: 409, Code: code, Message: msg}
}

// RuleInput 是新建 / 修改规则的请求体。
type RuleInput struct {
	Name        string               `json:"name"`
	Description string               `json:"description"`
	Priority    int                  `json:"priority"`
	Enabled     *bool                `json:"enabled"` // 新建时缺省启用
	Mode        string               `json:"mode"`    // observe / enforce；缺省 observe
	Scenes      []string             `json:"scenes"`
	Env         string               `json:"env"`
	InstanceIDs []uint               `json:"instance_ids"`
	GroupMode   string               `json:"group_mode"`
	GroupIDs    []uint               `json:"group_ids"`
	Match       string               `json:"match"`
	Conditions  []firewall.Condition `json:"conditions"`
	Action      string               `json:"action"`
	ActionArg   int                  `json:"action_arg"`
	Message     string               `json:"message"`
	Notify      bool                 `json:"notify"`
}

// RuleView 是规则的展示形态（列表与详情）。
type RuleView struct {
	ID           uint                 `json:"id"`
	Name         string               `json:"name"`
	Description  string               `json:"description"`
	Priority     int                  `json:"priority"`
	Enabled      bool                 `json:"enabled"`
	Mode         string               `json:"mode"`
	ObserveSince string               `json:"observe_since"`
	ObserveDays  int                  `json:"observe_days"` // 观察模式下：进入观察的第几天（含当天）；其他为 0
	Scenes       []string             `json:"scenes"`
	Env          string               `json:"env"`
	InstanceIDs  []uint               `json:"instance_ids"`
	GroupMode    string               `json:"group_mode"`
	GroupIDs     []uint               `json:"group_ids"`
	Match        string               `json:"match"`
	Conditions   []firewall.Condition `json:"conditions"`
	Action       string               `json:"action"`
	ActionArg    int                  `json:"action_arg"`
	Message      string               `json:"message"`
	Notify       bool                 `json:"notify"`
	UpdatedBy    string               `json:"updated_by"`
	UpdatedAt    string               `json:"updated_at"`
	CreatedAt    string               `json:"created_at"`
	Hits7        int64                `json:"hits7"` // 近 7 天命中次数（含观察）
	// 本进程自启动以来的求值统计（多实例部署各进程各自统计；重启清零）
	Evals      int64   `json:"evals"`
	AvgMs      float64 `json:"avg_ms"` // 平均每次求值耗时（含 EXPLAIN / 库表解析）
	MaxMs      float64 `json:"max_ms"`
	UnknownPct int     `json:"unknown_pct"`       // 有条件没能评估的次数占比（百分数）
	Slow       bool    `json:"slow"`              // 慢规则：求值次数足够且平均耗时超过预算
	Blind      bool    `json:"blind"`             // 未评估占比偏高：规则经常因为事实取不到而没起作用
	Spark      []int64 `json:"spark"`             // 近 7 天每天的命中次数，最后一个是今天
	Invalid    string  `json:"invalid,omitempty"` // 库里这条规则不能通过校验（手工改库所致）：不会生效
}

// RuleList 是规则列表。
type RuleList struct {
	Ready    bool       `json:"ready"`
	Missing  []string   `json:"missing,omitempty"` // 没建的表
	Rules    []RuleView `json:"rules"`
	MaxRules int        `json:"max_rules"`
}

// Overview 是概览：顶部统计与全局状态。
type Overview struct {
	Ready    bool     `json:"ready"`
	Missing  []string `json:"missing,omitempty"`
	Enabled  bool     `json:"enabled"`   // firewall.enabled（总开关）
	Mode     string   `json:"mode"`      // 全局模式：observe / enforce
	OnError  string   `json:"on_error"`  // allow / deny
	StoreSQL string   `json:"store_sql"` // masked / raw

	ExplainTimeoutMs int `json:"explain_timeout_ms"` // firewall.explain_timeout_ms 的生效值
	// 本进程里 EXPLAIN 事实的使用情况（没有规则用到预估扫描行数 / 本进程还没发起过 EXPLAIN 时为 nil）
	Explain    *firewall.ExplainStats `json:"explain,omitempty"`
	SlowRules  int                    `json:"slow_rules"`  // 慢规则数
	BlindRules int                    `json:"blind_rules"` // 未评估占比偏高的规则数

	RulesTotal     int `json:"rules_total"`
	RulesEnabled   int `json:"rules_enabled"`
	RulesObserving int `json:"rules_observing"` // 启用且处于观察模式
	RulesIdle      int `json:"rules_idle"`      // 启用、建了超过 7 天、近 7 天零命中

	HitsToday      int64 `json:"hits_today"`
	UsersToday     int64 `json:"users_today"`
	RulesHitToday  int64 `json:"rules_hit_today"`
	BlockedToday   int64 `json:"blocked_today"`    // 拒绝
	ReasonToday    int64 `json:"reason_today"`     // 要求原因
	TightenToday   int64 `json:"tighten_today"`    // 限行 / 强制脱敏
	WarnToday      int64 `json:"warn_today"`       // 仅告警
	ObserveToday   int64 `json:"observe_today"`    // 观察命中
	WouldDenyToday int64 `json:"would_deny_today"` // 其中若生效会被拒绝的
}

// GroupItem / InstanceItem 是编辑器与测试台的选项。
type GroupItem struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
}

type InstanceItem struct {
	ID     uint   `json:"id"`
	Name   string `json:"name"`
	DBType string `json:"db_type"`
	Env    string `json:"env"` // prod / test / ""
}

// SceneItem 描述一个场景及其在当前版本里是否已经接入执行。
type SceneItem struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Active bool   `json:"active"`
	Note   string `json:"note,omitempty"`
}

// EnvItem 是环境字典里的一项。
type EnvItem struct {
	Code string `json:"code"`
	Name string `json:"name"`
	Prod bool   `json:"is_prod"`
}

// Meta 是编辑器需要的选项与限制。
type Meta struct {
	Groups     []GroupItem    `json:"groups"`
	Instances  []InstanceItem `json:"instances"`
	Envs       []EnvItem      `json:"envs"` // 环境字典（平台设置 › 环境设置）
	Scenes     []SceneItem    `json:"scenes"`
	Features   []FeatureItem  `json:"features"`
	Pending    []string       `json:"pending_conditions"` // 可以配置但当前版本还不能评估（不会命中）的条件类型
	MaxRules   int            `json:"max_rules"`
	MaxConds   int            `json:"max_conditions"`
	BigInItems int            `json:"big_in_items"`
}

type FeatureItem struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// ModeResult 是单条规则切换观察 / 生效的结果。
type ModeResult struct {
	Rule     RuleView `json:"rule"`
	Observed int64    `json:"observed"`          // 近 7 天这条规则的命中次数
	Warning  string   `json:"warning,omitempty"` // 切到生效但没有观察数据时的提示
}

// HitView 是命中记录。
type HitView struct {
	ID          uint     `json:"id"`
	CreatedAt   string   `json:"created_at"`
	LastAt      string   `json:"last_at"`
	RuleID      uint     `json:"rule_id"`
	RuleName    string   `json:"rule_name"`
	RuleExists  bool     `json:"rule_exists"`
	Scene       string   `json:"scene"`
	User        string   `json:"user"`
	InstanceID  uint     `json:"instance_id"`
	Instance    string   `json:"instance"`
	DB          string   `json:"db"`
	Effective   string   `json:"effective"`
	WouldAction string   `json:"would_action"`
	Fingerprint string   `json:"fingerprint"`
	SQL         string   `json:"sql"`
	Reason      string   `json:"reason"`
	Repeat      int      `json:"repeat"`
	QueryLogID  uint     `json:"query_log_id"`
	Matched     []string `json:"matched"`
	Unknown     []string `json:"unknown"`
	Downgraded  string   `json:"downgraded"`
	Message     string   `json:"message"` // 规则当前的提示文案（用户当时看到的说明；规则被改过时可能不同）
}

// HitList 是命中记录的一页。
type HitList struct {
	Items []HitView `json:"items"`
	Total int64     `json:"total"`
	Page  int       `json:"page"`
	Size  int       `json:"size"`
}

// HitQuery 是命中记录的筛选条件。
type HitQuery struct {
	RuleID uint
	Q      string
	User   string
	Result string // "" 全部 / deny / reason / limit / mask / warn / observe
	Scene  string
	Days   int
	Page   int
	Size   int
}

// ExceptionView 是例外名单里的一项。
type ExceptionView struct {
	ID           uint   `json:"id"`
	RuleID       uint   `json:"rule_id"`
	RuleName     string `json:"rule_name"`
	SubjectKind  string `json:"subject_kind"`
	SubjectValue string `json:"subject_value"`
	SubjectText  string `json:"subject_text"`
	Reason       string `json:"reason"`
	ExpiresAt    string `json:"expires_at"`
	Expired      bool   `json:"expired"`
	CreatedBy    string `json:"created_by"`
	CreatedAt    string `json:"created_at"`
}

// ExceptionInput 是新建例外的请求体。
type ExceptionInput struct {
	RuleID       uint   `json:"rule_id"`
	SubjectKind  string `json:"subject_kind"`
	SubjectValue string `json:"subject_value"`
	Reason       string `json:"reason"`
	Days         int    `json:"days"`
}

// TemplateView 是规则模板。
type TemplateView struct {
	Key         string    `json:"key"`
	Category    string    `json:"category"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Rule        RuleInput `json:"rule"`
	Pending     bool      `json:"pending"` // 模板的场景当前版本都还没接入执行
	Applied     bool      `json:"applied"` // 已经有同名规则
}

// EvaluateRequest 是语句测试台的入参。
type EvaluateRequest struct {
	SQL        string `json:"sql"`
	Scene      string `json:"scene"`
	InstanceID uint   `json:"instance_id"` // 可选：选了实例就按它的环境与实例范围判断
	Env        string `json:"env"`         // 没选实例时手选：prod / test
	DB         string `json:"db"`
	User       string `json:"user"` // 可选：用来试“用户”类例外
	GroupIDs   []uint `json:"group_ids"`
	When       string `json:"when"`        // now / workday_day / workday_night / workday_late / weekend
	ScanRows   *int64 `json:"scan_rows"`   // 手填预估扫描行数（EXPLAIN 接入前用来试条件）；nil = 未知
	AffectRows *int64 `json:"affect_rows"` // 手填预估影响行数
}

// EvalHit 是测试台里的一条命中。
type EvalHit struct {
	Rule        firewall.RuleRef `json:"rule"`
	Effective   string           `json:"effective"`
	WouldAction string           `json:"would_action,omitempty"`
	Matched     []string         `json:"matched"`
	Unknown     []string         `json:"unknown"`
	Downgraded  string           `json:"downgraded,omitempty"`
	Highlight   []string         `json:"highlight"`
}

// EvalResult 是测试台的结果。
type EvalResult struct {
	Verdict     string                `json:"verdict"` // deny / reason / limit / mask / warn / pass
	Deny        bool                  `json:"deny"`
	Rule        *firewall.RuleRef     `json:"rule,omitempty"`
	Message     string                `json:"message"`
	NeedReason  bool                  `json:"need_reason"`
	ReasonMsgs  []string              `json:"reason_msgs"`
	MaxRows     int                   `json:"max_rows"`
	LimitedBy   string                `json:"limited_by"`
	ForceMask   []string              `json:"force_mask"`
	Warnings    []string              `json:"warnings"`
	Hits        []EvalHit             `json:"hits"`
	Skipped     []firewall.Skip       `json:"skipped"`
	Facts       firewall.FactsSummary `json:"facts"`
	GlobalMode  string                `json:"global_mode"`
	EnabledFlag bool                  `json:"enabled_flag"` // 总开关
	Caps        firewall.Caps         `json:"caps"`
	Highlight   []string              `json:"highlight"`
	Failed      bool                  `json:"failed"`
	Notes       []string              `json:"notes"`
}

// ReplaySample 是回放里的一条命中样例。
type ReplaySample struct {
	Scene    string   `json:"scene"`
	SQL      string   `json:"sql"`
	User     string   `json:"user"`
	Instance string   `json:"instance"`
	DB       string   `json:"db"`
	At       string   `json:"at"`
	Why      []string `json:"why"`
	Would    string   `json:"would"`
}

// ReplayResult 是历史回放的结果（设计 §10.1）。
type ReplayResult struct {
	Applicable  bool           `json:"applicable"` // 规则的场景里有在线查询（回放数据来自在线查询日志）
	Scanned     int            `json:"scanned"`
	ByScene     map[string]int `json:"scanned_by_scene"`
	Days        int            `json:"days"`
	Truncated   bool           `json:"truncated"` // 预算用完（条数 / 时间）
	Hits        int            `json:"hits"`
	Users       int            `json:"users"`
	DBAHits     int            `json:"dba_hits"`    // 来自 DBA 权限组的命中
	DBAKnown    bool           `json:"dba_known"`   // 找到了名为 DBA 的权限组
	Unevaluated int            `json:"unevaluated"` // 因事实缺失（EXPLAIN 类条件）没能评估的语句数
	Samples     []ReplaySample `json:"samples"`
	Rate        float64        `json:"rate"` // 命中占比（0–1）
	Advice      string         `json:"advice"`
	Notes       []string       `json:"notes"`
}

// ToggleResult 是总开关 / 全局模式的结果。
type SwitchResult struct {
	Enabled bool   `json:"enabled"`
	Mode    string `json:"mode"`
	Changed bool   `json:"changed"`
}

// ReplayRequest：回放一条已保存的规则（RuleID）或一份草稿（Rule）。
type ReplayRequest struct {
	RuleID uint       `json:"rule_id"`
	Rule   *RuleInput `json:"rule"`
	Days   int        `json:"days"`
}

type modeBody struct {
	Mode string `json:"mode"`
}

type switchBody struct {
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason"`
}

type toggleBody struct {
	Enabled bool `json:"enabled"`
}
