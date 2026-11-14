// @author: kevinkelly
// @license: Apache Licence
// @file: service.go
// @time: 2026/10/28

package maskadmin

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/engine"
	"dbtool/internal/masking"
	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// InstanceRepo 是需要的实例读取能力（由 server.Repo 提供）。
type InstanceRepo interface {
	ListInstances() ([]model.Instance, error)
	GetInstance(id uint) (*model.Instance, error)
	EngineConn(inst *model.Instance) engine.Conn
	ConfigValue(item string) (string, bool)
}

// Opener 按类型创建引擎，默认 engine.New。
type Opener func(dbType string, c engine.Conn) (engine.Engine, error)

// opKind 是一次保存里对存储的一个操作。
type opKind int

const (
	opInsertPolicy opKind = iota
	opUpdatePolicy
	opDeletePolicy
)

type storeOp struct {
	kind   opKind
	policy model.MaskingPolicy // opInsertPolicy / opUpdatePolicy
	id     uint                // opDeletePolicy 的策略 ID
	active bool
}

// Store 是脱敏配置的存储。
type Store interface {
	// PolicyReady 返回 dbt_masking_policy 是否已建。
	PolicyReady() bool
	ListPolicies(instanceID uint) ([]model.MaskingPolicy, error)
	// Counts 返回每个实例已启用的脱敏字段数。
	Counts() (map[uint]int, error)
	// Commit 在一个事务里执行 ops 并写一条审计。
	Commit(ops []storeOp, audit *model.AuditEntry) error
	// SetConfig 在一个事务里写 dbt_setting 的一项（没有则新增，已有则改值）并写一条审计。
	SetConfig(item, value, description string, audit *model.AuditEntry) error
}

type Service struct {
	repo InstanceRepo
	st   Store
	open Opener
	now  func() time.Time
}

func NewService(repo InstanceRepo, st Store, open Opener) *Service {
	if open == nil {
		open = engine.New
	}
	return &Service{repo: repo, st: st, open: open, now: time.Now}
}

func (s *Service) instance(id uint) (*model.Instance, error) {
	inst, err := s.repo.GetInstance(id)
	if err != nil || inst == nil {
		return nil, &Error{Status: 404, Code: "not_found", Message: "实例不存在"}
	}
	return inst, nil
}

func (s *Service) supportedInstance(id uint) (*model.Instance, error) {
	inst, err := s.instance(id)
	if err != nil {
		return nil, err
	}
	if !supported(inst.DBType) {
		return nil, &Error{Status: 400, Code: "unsupported", Message: fmt.Sprintf("该类型（%s）不在数据脱敏范围内：%s", inst.DBType, unsupportedReason(inst.DBType))}
	}
	return inst, nil
}

// Instances 列出全部实例（不支持脱敏的灰显并给出原因），带已配置字段数。
func (s *Service) Instances() ([]InstanceItem, error) {
	list, err := s.repo.ListInstances()
	if err != nil {
		return nil, err
	}
	counts, err := s.st.Counts()
	if err != nil {
		counts = map[uint]int{}
	}
	out := make([]InstanceItem, 0, len(list))
	for _, in := range list {
		it := InstanceItem{ID: in.ID, Name: in.InstanceName, DBType: in.DBType, Count: counts[in.ID]}
		if info := engineInfo(in.DBType); info != nil {
			it.Label = info.Label
		} else {
			it.Label = in.DBType
		}
		if supported(in.DBType) {
			it.Supported, it.Engine = true, engineInfo(in.DBType)
		} else {
			it.Why = unsupportedReason(in.DBType)
		}
		out = append(out, it)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Supported != out[j].Supported {
			return out[i].Supported
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (s *Service) withEngine(inst *model.Instance, fn func(engine.Engine) error) error {
	e, err := s.open(inst.DBType, s.repo.EngineConn(inst))
	if err != nil {
		return &Error{Status: 502, Code: "connect_failed", Message: fmt.Sprintf("连接实例失败：%s", err.Error())}
	}
	defer e.Close()
	return fn(e)
}

// Databases 返回实例上的库（MongoDB 的库、SQL Server / PostgreSQL 的库）。
func (s *Service) Databases(ctx context.Context, id uint) ([]string, error) {
	inst, err := s.supportedInstance(id)
	if err != nil {
		return nil, err
	}
	var dbs []string
	err = s.withEngine(inst, func(e engine.Engine) error {
		var err error
		dbs, err = e.AllDatabases(ctx)
		return err
	})
	if err != nil {
		if _, ok := err.(*Error); ok {
			return nil, err
		}
		return nil, &Error{Status: 502, Code: "engine_error", Message: fmt.Sprintf("读取库列表失败：%s", err.Error())}
	}
	sort.Strings(dbs)
	return dbs, nil
}

// Catalog 返回一个库的字段目录。
func (s *Service) Catalog(ctx context.Context, id uint, db string) (*Catalog, error) {
	db = strings.TrimSpace(db)
	if db == "" {
		return nil, badReq("请选择数据库")
	}
	inst, err := s.supportedInstance(id)
	if err != nil {
		return nil, err
	}
	var cat *Catalog
	err = s.withEngine(inst, func(e engine.Engine) error {
		var err error
		cat, err = catalogOf(ctx, e, inst, db)
		return err
	})
	if err != nil {
		if _, ok := err.(*Error); ok {
			return nil, err
		}
		return nil, &Error{Status: 502, Code: "engine_error", Message: err.Error()}
	}
	return cat, nil
}

// ---- 配置 ----

func ruleText(r RuleSpec) string {
	switch r.Kind {
	case masking.KindKeep:
		return fmt.Sprintf("保留前 %d 位 + *** + 后 %d 位", r.Prefix, r.Suffix)
	case masking.KindThree:
		return []string{"", "三段式：隐藏前段", "三段式：隐藏中段", "三段式：隐藏后段"}[min(max(r.Hide, 1), 3)]
	case masking.KindWhole:
		return "整体显示 ***"
	case masking.KindRegex:
		return fmt.Sprintf("正则：隐藏第 %d 组", r.Hide)
	}
	return "旧规则"
}

func sameKey(db1, tb1, f1, db2, tb2, f2 string) bool {
	return strings.EqualFold(db1, db2) && strings.EqualFold(tb1, tb2) && strings.EqualFold(f1, f2)
}

func timeText(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04")
}

// Config 返回实例的全部脱敏配置。
func (s *Service) Config(id uint) (*ConfigView, error) {
	if _, err := s.instance(id); err != nil {
		return nil, err
	}
	v := &ConfigView{Items: []ConfigItem{}, PolicyOK: s.st.PolicyReady()}
	v.MaskingOn = cfgTrue(s.repo, cfgMasking)
	v.QueryCheck = cfgTrue(s.repo, settings.KeyQueryBlockOnMaskError)
	v.ExportOn = v.MaskingOn && cfgDefaultTrue(s.repo, settings.KeyExportMaskEnabled)
	var pols []model.MaskingPolicy
	if v.PolicyOK {
		var err error
		if pols, err = s.st.ListPolicies(id); err != nil {
			return nil, err
		}
	}
	for _, p := range pols {
		r := RuleSpec{Kind: p.RuleKind, Prefix: p.KeepPrefix, Suffix: p.KeepSuffix, Hide: p.HideGroup, Regex: p.RuleRegex}
		v.Items = append(v.Items, ConfigItem{Source: "policy", ID: p.ID, ScopeDB: p.ScopeDB, ScopeTable: p.ScopeTable, Field: p.FieldPath,
			Wildcard: p.ScopeDB == "*" && p.ScopeTable == "*", Rule: r, RuleText: ruleText(r), Active: p.Active, UpdatedBy: p.UpdatedBy, UpdatedAt: timeText(p.UpdatedAt)})
	}
	return v, nil
}

// EnableMasking 打开系统脱敏总开关（dbt_setting 的 query.mask_enabled 置为 true），写系统审计。
// 本来就开着时不写库、不记审计，返回 Changed=false。只提供“开启”：关闭脱敏是降低保护的操作，不做成页面上的一键。
func (s *Service) EnableMasking(actor audit.Actor) (*ToggleResult, error) {
	if cfgTrue(s.repo, cfgMasking) {
		return &ToggleResult{MaskingOn: true}, nil
	}
	before := "未配置" // i18n-ignore 审计文本，存库后不翻译
	if v, ok := s.repo.ConfigValue(cfgMasking); ok {
		before = "原值 " + strings.TrimSpace(v) // i18n-ignore 审计文本，存库后不翻译
	}
	export := "开启" // i18n-ignore 审计文本，存库后不翻译
	if !cfgDefaultTrue(s.repo, settings.KeyExportMaskEnabled) {
		export = "已关闭，导出仍是原值" // i18n-ignore 审计文本，存库后不翻译
	}
	entry := &model.AuditEntry{UserID: actor.ID, UserName: actor.Username, UserDisplay: actor.Display, Action: "数据脱敏总开关", // i18n-ignore 审计文本，存库后不翻译
		ExtraInfo: fmt.Sprintf("开启系统脱敏总开关 %s（%s → true）；在线查询立即按脱敏配置脱敏，SQL 导出工单的脱敏开关 export.mask_enabled：%s。", cfgMasking, before, export)} // i18n-ignore 审计文本，存库后不翻译
	if err := s.st.SetConfig(cfgMasking, "true", "查询结果脱敏总开关", entry); err != nil { // i18n-ignore 审计文本，存库后不翻译
		return nil, err
	}
	return &ToggleResult{MaskingOn: true, Changed: true}, nil
}

// cfgMasking 是系统脱敏总开关在 dbt_setting 里的项名。
const cfgMasking = settings.KeyQueryMaskEnabled

func cfgTrue(r InstanceRepo, item string) bool {
	v, ok := r.ConfigValue(item)
	return ok && strings.EqualFold(strings.TrimSpace(v), "true")
}

func cfgDefaultTrue(r InstanceRepo, item string) bool {
	v, ok := r.ConfigValue(item)
	return !ok || !strings.EqualFold(strings.TrimSpace(v), "false")
}

// ---- 保存 ----

const (
	maxChanges = 2000
	maxDB      = 128
	maxTable   = 255
	maxField   = 255
)

func validName(s string, max int) bool {
	if s == "" || len([]rune(s)) > max {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// Save 把一批变更保存为脱敏策略。同一个字段已有的新策略被覆盖；旧配置里同名字段被停用（设置规则）或删除（移除）。
func (s *Service) Save(actor audit.Actor, req SaveRequest) (*SaveResult, error) {
	inst, err := s.supportedInstance(req.InstanceID)
	if err != nil {
		return nil, err
	}
	if !s.st.PolicyReady() {
		return nil, &Error{Status: 409, Code: "not_ready", Message: "还没有建脱敏策略表：请先在元数据库执行 deploy/sql/230_masking_policy.sql（或 dbt-admin bootstrap --only-sql --apply），然后重试。"}
	}
	if len(req.Changes) == 0 {
		return nil, badReq("没有要保存的变更")
	}
	if len(req.Changes) > maxChanges {
		return nil, badReq(fmt.Sprintf("一次最多保存 %d 项变更", maxChanges))
	}
	// 校验并去重（后面的覆盖前面的）
	type key struct{ db, tb, f string }
	norm := map[key]Change{}
	var order []key
	for i, ch := range req.Changes {
		ch.ScopeDB, ch.ScopeTable, ch.Field = strings.TrimSpace(ch.ScopeDB), strings.TrimSpace(ch.ScopeTable), strings.TrimSpace(ch.Field)
		wild := ch.ScopeDB == "*" || ch.ScopeTable == "*"
		if wild && !(ch.ScopeDB == "*" && ch.ScopeTable == "*") {
			return nil, badReq(fmt.Sprintf("第 %d 项：库与表要么都是 *（任意库表的同名字段），要么都写具体名字", i+1))
		}
		if !wild && (!validName(ch.ScopeDB, maxDB) || !validName(ch.ScopeTable, maxTable)) {
			return nil, badReq(fmt.Sprintf("第 %d 项：库名 / 表名不合法", i+1))
		}
		if !validName(ch.Field, maxField) {
			return nil, badReq(fmt.Sprintf("第 %d 项：字段名不合法", i+1))
		}
		switch ch.Op {
		case "set":
			if ch.Rule == nil {
				return nil, badReq(fmt.Sprintf("第 %d 项：缺少脱敏规则", i+1))
			}
			if err := ch.Rule.toRule().Validate(); err != nil {
				return nil, badReq(fmt.Sprintf("第 %d 项（%s）：%v", i+1, ch.Field, err))
			}
		case "remove", "enable", "disable":
		default:
			return nil, badReq(fmt.Sprintf("第 %d 项：不认识的操作 \"%s\"", i+1, ch.Op))
		}
		k := key{strings.ToLower(ch.ScopeDB), strings.ToLower(ch.ScopeTable), strings.ToLower(ch.Field)}
		if _, ok := norm[k]; !ok {
			order = append(order, k)
		}
		norm[k] = ch
	}

	pols, err := s.st.ListPolicies(inst.ID)
	if err != nil {
		return nil, err
	}
	var ops []storeOp
	res := &SaveResult{}
	var lines []string
	now := s.now()
	for _, k := range order {
		ch := norm[k]
		var pol *model.MaskingPolicy
		for i := range pols {
			if sameKey(pols[i].ScopeDB, pols[i].ScopeTable, pols[i].FieldPath, ch.ScopeDB, ch.ScopeTable, ch.Field) {
				pol = &pols[i]
				break
			}
		}
		where := ch.ScopeDB + "." + ch.ScopeTable + "." + ch.Field
		if ch.ScopeDB == "*" {
			where = "*." + ch.Field
		}
		switch ch.Op {
		case "set":
			p := model.MaskingPolicy{InstanceID: inst.ID, ScopeDB: ch.ScopeDB, ScopeTable: ch.ScopeTable, FieldPath: ch.Field,
				RuleKind: ch.Rule.Kind, KeepPrefix: ch.Rule.Prefix, KeepSuffix: ch.Rule.Suffix, HideGroup: ch.Rule.Hide, RuleRegex: ch.Rule.Regex,
				Active: true, UpdatedBy: actor.Username, UpdatedAt: now}
			if pol != nil {
				p.ID, p.CreatedAt = pol.ID, pol.CreatedAt
				ops = append(ops, storeOp{kind: opUpdatePolicy, policy: p})
				res.Updated++
				lines = append(lines, "覆盖 "+where+" → "+ruleText(*ch.Rule)) // i18n-ignore 审计文本，存库后不翻译
			} else {
				p.CreatedAt = now
				ops = append(ops, storeOp{kind: opInsertPolicy, policy: p})
				res.Added++
				lines = append(lines, "新增 "+where+" → "+ruleText(*ch.Rule)) // i18n-ignore 审计文本，存库后不翻译
			}
		case "remove":
			n := 0
			if pol != nil {
				ops = append(ops, storeOp{kind: opDeletePolicy, id: pol.ID})
				n++
			}
			if n > 0 {
				res.Removed++
				lines = append(lines, "移除 "+where) // i18n-ignore 审计文本，存库后不翻译
			}
		case "enable", "disable":
			on := ch.Op == "enable"
			n := 0
			if pol != nil {
				p := *pol
				p.Active, p.UpdatedBy, p.UpdatedAt = on, actor.Username, now
				ops = append(ops, storeOp{kind: opUpdatePolicy, policy: p})
				n++
			}
			if n > 0 {
				res.Toggled++
				lines = append(lines, map[bool]string{true: "启用 ", false: "停用 "}[on]+where) // i18n-ignore 审计文本，存库后不翻译
			}
		}
	}
	if len(ops) == 0 {
		return res, nil
	}
	extra := fmt.Sprintf("实例 %s：新增 %d、覆盖 %d、移除 %d、启停 %d。", inst.InstanceName, res.Added, res.Updated, res.Removed, res.Toggled) // i18n-ignore 审计文本，存库后不翻译
	if len(lines) > 12 {
		extra += strings.Join(lines[:12], "；") + fmt.Sprintf("；…另 %d 项", len(lines)-12) // i18n-ignore 审计文本，存库后不翻译
	} else {
		extra += strings.Join(lines, "；")
	}
	entry := &model.AuditEntry{UserID: actor.ID, UserName: actor.Username, UserDisplay: actor.Display, Action: "数据脱敏配置变更", ExtraInfo: extra} // i18n-ignore 审计文本，存库后不翻译
	if err := s.st.Commit(ops, entry); err != nil {
		return nil, err
	}
	return res, nil
}
