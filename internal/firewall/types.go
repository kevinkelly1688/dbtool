// @author: kevinkelly
// @license: Apache Licence
// @file: types.go
// @time: 2026/10/28

// Package firewall 是规则式的 SQL 语句管控（“SQL 防火墙”）：管理员配置规则，语句进入在线查询 / 工单 / 导出之前先过一遍规则，
// 得到放行 / 拒绝 / 要求填写原因 / 限制行数 / 强制脱敏 / 仅告警的决定，并留下命中记录。
//
// 分层：
//   - 本包的求值是纯函数（Evaluate 不写库），命中记录由调用方在拿到决定之后用 Record 写入；
//   - 事实（语句类型、结构特征、涉及对象、指纹）按需提取，缺失的事实让条件“未知”，未知按“不命中”处理并写进命中说明；
//   - 多条规则按“最严”合并：任一 deny 即拒绝；limit 取最小、mask 取并集、reason 任一即需要；warn 放行并记录；
//   - 规则可以处于“观察”模式（只记录、不执行），全局也有观察闸门。
//
// 本包不依赖 internal/query、internal/sqlflow（两者依赖它）。
package firewall

import "time"

// 场景。
const (
	SceneQuery    = "query"    // 在线查询
	SceneWorkflow = "workflow" // SQL 工单（提交检查 / 执行前复核）
	SceneExport   = "export"   // 数据导出
)

// Scenes 是全部场景（顺序即页面显示顺序）。
var Scenes = []string{SceneQuery, SceneWorkflow, SceneExport}

// 动作。第一期不含升级为工单审批（escalate）。
const (
	ActDeny   = "deny"   // 拒绝
	ActReason = "reason" // 要求填写原因
	ActLimit  = "limit"  // 限制返回行数
	ActMask   = "mask"   // 强制脱敏
	ActWarn   = "warn"   // 仅告警
)

// Actions 是全部动作（由严到宽）。
var Actions = []string{ActDeny, ActReason, ActLimit, ActMask, ActWarn}

// 规则模式。
const (
	ModeObserve = "observe" // 只记录命中，不执行动作
	ModeEnforce = "enforce" // 执行动作
)

// 条件类型。
const (
	CondStmt        = "stmt"
	CondFeature     = "feature"
	CondKeyword     = "keyword"
	CondRegex       = "regex"
	CondObject      = "object"
	CondScanRows    = "scan_rows"
	CondAffectRows  = "affect_rows"
	CondTime        = "time"
	CondFingerprint = "fingerprint"
)

// 结构特征。
const (
	FeatNoWhere         = "no_where"
	FeatNoLimit         = "no_limit"
	FeatCartesian       = "cartesian"
	FeatMultiStmt       = "multi_stmt"
	FeatSelectStar      = "select_star"
	FeatLeadingWildcard = "leading_wildcard"
	FeatBigIn           = "big_in"
)

// BigInItems 是 big_in 特征的阈值：IN 列表项数超过它。
const BigInItems = 1000

// 限制。
const (
	MaxRules       = 500 // 规则总数上限
	MaxConditions  = 20  // 每条规则的条件数上限
	MaxNameLen     = 100 // 规则名称
	MaxTextLen     = 500 // 说明 / 提示文案
	MaxPatternLen  = 500 // 正则 / 对象模式的长度
	MaxListItems   = 50  // 关键字 / 对象模式列表的项数
	MaxLimitRows   = 1000000
	maxFactsBytes  = 64 << 10 // 超长语句只对前 64KB 提取事实
	MaxHitSQLRunes = 2000
)

// 命中记录的合并窗口：同一 (规则, 用户, 指纹, 结果) 在窗口内只留一行，后续命中累加 repeat_cnt。
const HitMergeWindow = 10 * time.Minute

// Condition 是规则的一个命中条件，v 的形态由 t 决定：
//
//	stmt: ["UPDATE","DELETE"]   feature: "no_where"   keyword: ["SLEEP","INTO OUTFILE"]   regex: "…"
//	object: ["*.sys_users","shop.orders.phone"]   scan_rows / affect_rows: 1000000   time: "offhours"   fingerprint: "a1b2c3d4"
type Condition struct {
	T string `json:"t"`
	V any    `json:"v"`
}

// Rule 是一条防火墙规则（领域对象；库里的形态见 model.FirewallRule）。
type Rule struct {
	ID           uint
	Name         string
	Description  string
	Priority     int
	Enabled      bool
	Mode         string
	ObserveSince *time.Time
	Scenes       []string
	Env          string // all / prod / test
	InstanceIDs  []uint
	GroupMode    string // all / only / except
	GroupIDs     []uint
	Match        string // all / any
	Conditions   []Condition
	Action       string
	ActionArg    int
	Message      string
	Notify       bool
	UpdatedBy    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Exception 是对某条规则的豁免。
type Exception struct {
	ID           uint
	RuleID       uint
	SubjectKind  string // user / group / instance / fingerprint
	SubjectValue string
	Reason       string
	ExpiresAt    time.Time
	CreatedBy    string
}

// 例外的主体类型。
const (
	SubjectUser        = "user"
	SubjectGroup       = "group"
	SubjectInstance    = "instance"
	SubjectFingerprint = "fingerprint"
)

// Object 是语句里涉及的一个对象（库.表）。Schema 为空表示语句里没写库名（求值时用请求里的当前库补全）。
type Object struct {
	Schema string
	Table  string
}

// ObjectRef 是强制脱敏的目标：库.表.列；库与表都是 * 表示“任意库表里的这个列”。
type ObjectRef struct {
	Schema string
	Table  string
	Column string
}

// Caps 说明调用方能执行哪些“收紧”动作；不能执行的动作在合并时降级为仅告警（并在命中说明里注明）。
type Caps struct {
	Reason bool // 能向用户收集原因（在线查询的前端会弹窗；工单 / 导出没有）
	Limit  bool // 能收紧返回行数
	Mask   bool // 能强制脱敏
}

// FullCaps 是在线查询（MySQL 系 / SQL Server）的能力。
var FullCaps = Caps{Reason: true, Limit: true, Mask: true}

// Request 是一次求值的输入。
type Request struct {
	Scene string
	// 用户。GroupIDs 为 nil 时由 Engine 通过 Store.UserGroupIDs 读取。
	UserID   uint
	User     string
	GroupIDs []uint
	// 实例与库。Env 由调用方按实例标签推断（all 之外的取值：prod / test；未知填空）。
	InstanceID   uint
	InstanceName string
	DBType       string
	Env          string
	DB           string
	// 要求值的语句（改写 LIMIT 之前的用户语句）。
	SQL string
	// 用户已经填写的原因（reason 动作用）。
	Reason string
	Caps   Caps
	// Tables 返回语句涉及的对象（goInception 语法树 / 词法解析）；nil 表示只能用词法。出错 = object 条件未知。
	Tables func() ([]Object, error)
	// AffectRows 是预估影响行数（工单检查结果里已有）；< 0 表示未知。
	AffectRows int64
	// ScanRows 取预估扫描行数（EXPLAIN）；nil 表示不可用。第一期不接，scan_rows 条件未知。
	ScanRows func() (int64, error)
	// Now 为零值时取当前时间（测试注入）。
	Now time.Time
}

// RuleRef 是对规则的轻量引用（命中记录与决定里用）。
type RuleRef struct {
	ID       uint   `json:"id"`
	Name     string `json:"name"`
	Priority int    `json:"priority"`
	Action   string `json:"action"`
	Mode     string `json:"mode"`
	Message  string `json:"message,omitempty"`
}

// Hit 是一条规则对本次语句的命中。
type Hit struct {
	Rule RuleRef `json:"rule"`
	// Effective 是实际生效的动作：deny / reason / limit / mask / warn，或 observe（观察态，不执行）。
	Effective string `json:"effective"`
	// WouldAction 仅 Effective=observe 时有值：若规则生效会执行的动作。
	WouldAction string `json:"would_action,omitempty"`
	// Matched 是命中的条件说明（给命中详情与测试台看）；Unknown 是未能评估的条件及原因。
	Matched []string `json:"matched,omitempty"`
	Unknown []string `json:"unknown,omitempty"`
	// Downgraded 非空表示动作被降级为 warn 及原因（调用方没有这个能力）。
	Downgraded string `json:"downgraded,omitempty"`
	// ID 是写入命中表后的行 ID（Record 之后才有）。
	ID uint `json:"id,omitempty"`
}

// Skip 是被跳过的规则（范围不符 / 例外），测试台与命中详情里解释“为什么没命中”。
type Skip struct {
	Rule   RuleRef `json:"rule"`
	Reason string  `json:"reason"`
	// Unknown 为真表示规则在范围内、也没有被例外豁免，但有条件因事实缺失没能评估（历史回放据此统计“未评估”）。
	Unknown bool `json:"unknown,omitempty"`
}

// Facts 是语句事实的摘要（命中详情 / 测试台展示）。
type FactsSummary struct {
	Kind        string   `json:"kind"`        // SELECT / UPDATE / … ；其他语句为首个关键字的大写
	Objects     []string `json:"objects"`     // 库.表
	Fingerprint string   `json:"fingerprint"` // 8 位十六进制
	Multi       bool     `json:"multi"`
}

// Decision 是一次求值的结果。最终决定按“最严”合并，规则见 merge。
type Decision struct {
	Deny       bool        // 拒绝
	Rule       *RuleRef    // 决定拒绝的那条规则（多条 deny 取 priority 最小的）
	Message    string      // 给用户看的说明
	NeedReason bool        // 需要用户填写原因（调用方 Request.Reason 为空时）
	ReasonMsg  []string    // 各条 reason 规则的提示文案（去重）
	MaxRows    int         // > 0：本次最多返回这么多行（各 limit 取最小）
	ForceMask  []ObjectRef // 强制脱敏的对象（各 mask 的并集）
	Warnings   []string    // 仅告警的提示文案（放行，页面顶部提示）
	LimitedBy  string      // 决定 MaxRows 的规则名
	Hits       []Hit
	Skipped    []Skip
	Facts      FactsSummary
	// Failed 表示求值本身出错（读规则失败 / panic）；Deny 是否为真取决于 firewall.on_error。
	Failed bool
}

// Executed 返回会实际生效的命中（不含观察态）。
func (d *Decision) Executed() []Hit {
	var out []Hit
	for _, h := range d.Hits {
		if h.Effective != "observe" {
			out = append(out, h)
		}
	}
	return out
}
