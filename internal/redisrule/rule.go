// @author: kevinkelly
// @license: Apache Licence
// @file: rule.go
// @time: 2026/10/28

// Package redisrule 是 Redis 工单上线的“管理员规则”：规则模型与校验、命令匹配（白名单语义）、glob 键匹配、规则存储。
//
// 谁能做什么：
//   - 规则只有超级用户能定义（页面与接口在 internal/redisrule/admin，服务端校验）；
//   - 一条命令必须同时命中某条“启用”规则的命令名、键模式、适用范围、键数上限才允许；没有命中的一律拒绝，没有规则 = 全部拒绝；
//   - 硬禁止命令（FLUSHALL、CONFIG、EVAL、KEYS……）与所有无键命令，规则无法放行，规则编辑器里选这些命令直接报错（见 internal/engine/redis 的 cmdkeys.go）。
//
// 本包不依赖 server / sqlflow，两者都依赖它。
package redisrule

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"dbtool/internal/engine/redis"
)

// 规则字段的上限。
const (
	MaxNameLen    = 50
	MaxCommands   = 40
	MaxPatterns   = 20
	MaxPatternLen = 200
	MaxScopeItems = 50
	MaxKeysLimit  = 10000 // 单条命令影响键数上限的上限
	DefaultMaxKey = 100
	MaxTextLen    = 500
	MaxRules      = 500 // 规则总数上限
)

// Rule 是一条上线规则。
type Rule struct {
	ID           uint      `json:"id"`
	Name         string    `json:"name"`
	Enabled      bool      `json:"enabled"`
	Commands     []string  `json:"commands"`      // 大写命令名
	KeyPatterns  []string  `json:"key_patterns"`  // glob（Redis 风格：* ? [abc] [^a] [a-z] \转义）
	InstanceIDs  []uint    `json:"instance_ids"`  // 为空 = 全部实例
	DBs          []int     `json:"dbs"`           // 为空 = 全部库
	MaxKeys      int       `json:"max_keys"`      // 单条命令涉及的键数上限
	RecordBefore bool      `json:"record_before"` // 执行前记录被改键的原值（明文显示在工单详情里）
	Description  string    `json:"description"`
	Remark       string    `json:"remark"`
	CreatedBy    string    `json:"created_by"`
	UpdatedBy    string    `json:"updated_by"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Error 是规则校验错误（面向管理员的提示）。
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &Error{Msg: fmt.Sprintf(format, a...)} }

// Normalize 校验并规整一条规则的用户输入（ID / 创建人等由存储层填）。命令名转大写去重，键模式去重，范围去重排序。
func Normalize(in Rule) (Rule, error) {
	r := in
	r.Name = strings.TrimSpace(r.Name)
	r.Description = strings.TrimSpace(r.Description)
	r.Remark = strings.TrimSpace(r.Remark)
	switch {
	case r.Name == "":
		return r, invalid("请填写规则名称")
	case len([]rune(r.Name)) > MaxNameLen:
		return r, invalid("规则名称过长（最多 %d 个字符）", MaxNameLen)
	case len([]rune(r.Description)) > MaxTextLen:
		return r, invalid("说明过长（最多 %d 个字符）", MaxTextLen)
	case len([]rune(r.Remark)) > MaxTextLen:
		return r, invalid("备注过长（最多 %d 个字符）", MaxTextLen)
	}

	// 命令
	seen := map[string]bool{}
	var cmds []string
	for _, c := range r.Commands {
		for _, part := range strings.FieldsFunc(c, func(x rune) bool { return x == ',' || x == ' ' || x == '\n' || x == '\t' || x == '，' }) {
			name := strings.ToUpper(part)
			if seen[name] {
				continue
			}
			if err := checkCommandName(name); err != nil {
				return r, err
			}
			seen[name] = true
			cmds = append(cmds, name)
		}
	}
	if len(cmds) == 0 {
		return r, invalid("请至少选择一个命令")
	}
	if len(cmds) > MaxCommands {
		return r, invalid("一条规则最多 %d 个命令", MaxCommands)
	}
	sort.Strings(cmds)
	r.Commands = cmds

	// 键模式
	seen = map[string]bool{}
	var pats []string
	for _, p := range r.KeyPatterns {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		if err := ValidatePattern(p); err != nil {
			return r, err
		}
		seen[p] = true
		pats = append(pats, p)
	}
	if len(pats) == 0 {
		return r, invalid("请至少填写一个键匹配模式（如 cache:*）")
	}
	if len(pats) > MaxPatterns {
		return r, invalid("一条规则最多 %d 个键匹配模式", MaxPatterns)
	}
	r.KeyPatterns = pats

	// 适用范围
	ids := map[uint]bool{}
	var iids []uint
	for _, id := range r.InstanceIDs {
		if id == 0 {
			return r, invalid("实例编号不合法")
		}
		if !ids[id] {
			ids[id] = true
			iids = append(iids, id)
		}
	}
	if len(iids) > MaxScopeItems {
		return r, invalid("最多限定 %d 个实例", MaxScopeItems)
	}
	sort.Slice(iids, func(i, j int) bool { return iids[i] < iids[j] })
	r.InstanceIDs = iids
	dbs := map[int]bool{}
	var dlist []int
	for _, d := range r.DBs {
		if d < 0 || d > 1023 {
			return r, invalid("库编号 %d 不合法（0 ~ 1023）", d)
		}
		if !dbs[d] {
			dbs[d] = true
			dlist = append(dlist, d)
		}
	}
	if len(dlist) > MaxScopeItems {
		return r, invalid("最多限定 %d 个库", MaxScopeItems)
	}
	sort.Ints(dlist)
	r.DBs = dlist

	// 键数上限
	if r.MaxKeys == 0 {
		r.MaxKeys = DefaultMaxKey
	}
	if r.MaxKeys < 1 || r.MaxKeys > MaxKeysLimit {
		return r, invalid("单条命令影响键数上限需在 1 ~ %d 之间", MaxKeysLimit)
	}
	return r, nil
}

// checkCommandName 规则里只能选“受支持的、有键的写命令”；硬禁止命令给出明确原因。
func checkCommandName(name string) error {
	if len(name) > 30 {
		return invalid("命令名过长：%s", name[:30])
	}
	for _, c := range name {
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return invalid("命令名只能是字母、数字、下划线：%s", name)
		}
	}
	if redis.IsHardForbiddenName(name) {
		return invalid("命令 %s 属于硬禁止命令，任何规则都无法放行，不能写入规则", name)
	}
	if redis.LookupCmd(name) == nil {
		return invalid("命令 %s 不在受支持的写命令清单里（无键命令、读命令与未知命令不能写入规则）", name)
	}
	return nil
}

// CheckCommandName 对外暴露，供页面即时校验。
func CheckCommandName(name string) error {
	return checkCommandName(strings.ToUpper(strings.TrimSpace(name)))
}

// InScope 判断规则是否适用于该实例与库。
func (r *Rule) InScope(instanceID uint, db int) bool {
	if len(r.InstanceIDs) > 0 {
		ok := false
		for _, id := range r.InstanceIDs {
			if id == instanceID {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if len(r.DBs) > 0 {
		ok := false
		for _, d := range r.DBs {
			if d == db {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// HasCommand 规则是否包含该命令（name 大小写不敏感）。
func (r *Rule) HasCommand(name string) bool {
	name = strings.ToUpper(name)
	for _, c := range r.Commands {
		if c == name {
			return true
		}
	}
	return false
}

// ScopeText 把适用范围写成一句话。
func (r *Rule) ScopeText() string {
	dbs := ""
	if len(r.DBs) > 0 {
		parts := make([]string, len(r.DBs))
		for i, d := range r.DBs {
			parts[i] = fmt.Sprintf("db%d", d)
		}
		dbs = strings.Join(parts, "、")
	}
	// 四种组合各写成整句，便于按目录翻译（拼接起来的碎片没法整句匹配）
	switch {
	case len(r.InstanceIDs) == 0 && dbs == "":
		return "全部实例 / 全部库"
	case len(r.InstanceIDs) == 0:
		return fmt.Sprintf("全部实例 / %s", dbs)
	case dbs == "":
		return fmt.Sprintf("%d 个指定实例 / 全部库", len(r.InstanceIDs))
	}
	return fmt.Sprintf("%d 个指定实例 / %s", len(r.InstanceIDs), dbs)
}
