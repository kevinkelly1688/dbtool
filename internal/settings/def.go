// @author: kevinkelly
// @license: Apache Licence
// @file: def.go
// @time: 2026/10/28

// Package settings 是 dbtool 自己的系统设置体系：统一登记（类型、默认值、校验、分组）、统一存储（dbt_setting）、
// 统一读取（带缓存的服务）、统一修改（校验、加密、变更记录、审计）。设计见 docs/settings-design.md。
package settings

import "strings"

// Type 是设置值的类型，决定校验方式与页面控件。值一律以字符串存储。
type Type string

const (
	TypeBool   Type = "bool"   // true / false
	TypeInt    Type = "int"    // 整数
	TypeFloat  Type = "float"  // 小数
	TypeString Type = "string" // 单行文本
	TypeText   Type = "text"   // 多行文本（JSON 等）
	TypeEnum   Type = "enum"   // 取值只能是 Options 之一
	TypeList   Type = "list"   // 逗号分隔的列表
	TypeSecret Type = "secret" // 敏感值：加密保存，页面只显示“已设置”
)

// Option 是枚举的一个取值。
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Link 是设置对应的专门页面（页面里有比单个设置更丰富的操作，如连接测试、规则编辑）。
type Link struct {
	Label string `json:"label"`
	Href  string `json:"href"`
}

// Def 是一个设置的登记信息。
type Def struct {
	Key     string `json:"key"`
	Group   string `json:"group"`
	Section string `json:"section"`
	Label   string `json:"label"`
	Help    string `json:"help,omitempty"`
	Type    Type   `json:"type"`
	Default string `json:"default"`
	Unit    string `json:"unit,omitempty"`

	Min     float64            `json:"min,omitempty"`
	Max     float64            `json:"max,omitempty"`
	Ranged  bool               `json:"ranged,omitempty"`
	Options []Option           `json:"options,omitempty"`
	Check   func(string) error `json:"-"`

	Risk   string `json:"risk,omitempty"`   // 非空 = 高危项：页面保存时要求键入项名确认
	Locked string `json:"locked,omitempty"` // 非空 = 页面里不能改（原因）；可用 dbt-admin settings set --force 修改
	Hidden bool   `json:"hidden,omitempty"` // 运行状态类，不在页面显示，也不能在页面修改
	Link   *Link  `json:"link,omitempty"`

	Placeholder string `json:"placeholder,omitempty"`
}

// Secret 表示该项是敏感值（加密保存、不回显）。
func (d *Def) Secret() bool { return d.Type == TypeSecret }

// Group 是页面上的一个分组。
type Group struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Desc  string `json:"desc,omitempty"`
}

// ---- 登记用的链式构造（只在 registry.go 里使用）----

func newDef(key, group, section, label string, t Type, def string) *Def {
	return &Def{Key: key, Group: group, Section: section, Label: label, Type: t, Default: def}
}

func (d *Def) help(s string) *Def           { d.Help = s; return d }
func (d *Def) unit(s string) *Def           { d.Unit = s; return d }
func (d *Def) rng(lo, hi float64) *Def      { d.Min, d.Max, d.Ranged = lo, hi, true; return d }
func (d *Def) risk(s string) *Def           { d.Risk = s; return d }
func (d *Def) lock(s string) *Def           { d.Locked = s; return d }
func (d *Def) hide() *Def                   { d.Hidden = true; return d }
func (d *Def) link(label, href string) *Def { d.Link = &Link{Label: label, Href: href}; return d }
func (d *Def) check(f func(string) error) *Def {
	d.Check = f
	return d
}
func (d *Def) hint(s string) *Def { d.Placeholder = s; return d }

// opts 设置枚举取值：参数是 "值" 或 "值=说明"。
func (d *Def) opts(pairs ...string) *Def {
	for _, p := range pairs {
		v, l, _ := strings.Cut(p, "=")
		if l == "" {
			l = v
		}
		d.Options = append(d.Options, Option{Value: v, Label: l})
	}
	return d
}
