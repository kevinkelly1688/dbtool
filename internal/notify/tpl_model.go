// @author: kevinkelly
// @license: Apache Licence
// @file: tpl_model.go
// @time: 2026/10/28

package notify

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 本文件：消息模板的数据结构与校验。模板按“消息（事件键）× 范围（全局 / 渠道专属）”保存，由管理员在
// “通知渠道 › 消息模板”里维护；发送时渠道专属优先，其次全局，都没有就沿用系统默认（事件里现成的 Title / Content）。

// 内容块类型。
const (
	BlockFields  = "fields"  // 字段表：一行一个“名称：值”，可勾选“占半行”
	BlockText    = "text"    // 一段文字
	BlockCode    = "code"    // SQL / 代码块
	BlockLink    = "link"    // 链接 / 按钮
	BlockDivider = "divider" // 分隔线
)

// SQL / 代码块的显示方式。
const (
	ModeHead = "head" // 只显示前 N 个字
	ModeMask = "mask" // 隐藏字符串常量（'…' → '***'），再按 N 截断
	ModeFull = "full" // 完整显示（受渠道上限限制）
	ModeNone = "none" // 不显示，只留一句说明
)

// 状态色。
const (
	ToneInfo = "info"
	ToneOK   = "ok"
	ToneWarn = "warn"
	ToneCrit = "crit"
)

// 范围。
const ScopeGlobal = "global"

// ScopeChannel 返回渠道专属范围的键。
func ScopeChannel(id uint) string { return fmt.Sprintf("c:%d", id) }

// ChannelOfScope 解析范围键里的渠道 ID；全局或非法返回 0。
func ChannelOfScope(scope string) uint {
	if !strings.HasPrefix(scope, "c:") {
		return 0
	}
	var n uint
	if _, err := fmt.Sscanf(scope[2:], "%d", &n); err != nil || fmt.Sprintf("c:%d", n) != scope {
		return 0
	}
	return n
}

// 上限。
const (
	MaxBlocks     = 20
	MaxRows       = 20
	MaxNameRunes  = 60
	MaxTitleRunes = 200
	MaxIconRunes  = 8
	MaxLabelRunes = 40
	MaxValueRunes = 500
	MaxTextRunes  = 2000
	MaxTplJSON    = 16 << 10 // blocks 序列化后的字节上限
	DefaultCodeN  = 500
	MinCodeN      = 50
	MaxCodeN      = 3000
	maxSQLVar     = 20000 // 事件里带的 SQL 全文上限（字符）
)

// Row 是字段表里的一行。
type Row struct {
	L     string `json:"l"`
	V     string `json:"v"`
	Short bool   `json:"short,omitempty"`
}

// Block 是一个内容块；按 T 取用不同的字段。
type Block struct {
	T    string `json:"t"`
	Rows []Row  `json:"rows,omitempty"` // fields
	L    string `json:"l,omitempty"`    // code / link 的名称（小标题 / 链接文字）
	V    string `json:"v,omitempty"`    // text 的内容；code 的内容；link 的地址
	Mode string `json:"mode,omitempty"` // code
	N    int    `json:"n,omitempty"`    // code：head / mask 的字数
	As   string `json:"as,omitempty"`   // link：button / text
}

// Template 是一条消息模板。
type Template struct {
	EventKey  string  `json:"event"`
	Scope     string  `json:"scope"` // global / c:<渠道 ID>
	Name      string  `json:"name"`
	Enabled   bool    `json:"enabled"`
	Title     string  `json:"title"`
	Tone      string  `json:"tone"`
	Icon      string  `json:"icon"`
	Blocks    []Block `json:"blocks"`
	UpdatedBy string  `json:"updated_by,omitempty"`
	UpdatedAt string  `json:"updated_at,omitempty"`
}

// Clone 返回深拷贝。
func (t Template) Clone() Template {
	c := t
	c.Blocks = make([]Block, len(t.Blocks))
	for i, b := range t.Blocks {
		c.Blocks[i] = b
		c.Blocks[i].Rows = append([]Row(nil), b.Rows...)
	}
	return c
}

func validTone(s string) bool { return s == ToneInfo || s == ToneOK || s == ToneWarn || s == ToneCrit }

func badCtl(s string, allowNL bool) bool {
	for _, r := range s {
		if r == '\n' && allowNL {
			continue
		}
		if r == '\r' && allowNL {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// Issue 是校验里的一条问题。
type Issue struct {
	Level string `json:"level"` // crit：不能保存；warn：提示
	Text  string `json:"text"`
}

// Normalize 补默认值并整理字符串（去首尾空白、补 N、补 as），不改变语义。
func (t *Template) Normalize() {
	t.Name = strings.TrimSpace(t.Name)
	t.Title = strings.TrimSpace(t.Title)
	t.Icon = strings.TrimSpace(t.Icon)
	if t.Tone == "" {
		t.Tone = ToneInfo
	}
	for i := range t.Blocks {
		b := &t.Blocks[i]
		switch b.T {
		case BlockCode:
			if b.Mode == "" {
				b.Mode = ModeMask
			}
			if b.N == 0 {
				b.N = DefaultCodeN
			}
		case BlockLink:
			if b.As == "" {
				b.As = "button"
			}
		}
	}
}

// Validate 校验模板：结构、长度、变量。def 为该消息的定义（可为 nil：未知消息只做结构校验）。
// 返回的问题里有 crit 就不能保存；warn 只是提示（例如用了这条消息里没有的变量，发出去会是空的）。
func (t *Template) Validate(def *MessageDef) []Issue {
	var out []Issue
	bad := func(format string, a ...any) { out = append(out, Issue{"crit", fmt.Sprintf(format, a...)}) }
	warn := func(format string, a ...any) { out = append(out, Issue{"warn", fmt.Sprintf(format, a...)}) }

	if utf8.RuneCountInString(t.Name) > MaxNameRunes {
		bad("模板名称最长 %d 个字符", MaxNameRunes)
	}
	if badCtl(t.Name, false) {
		bad("模板名称里不能有换行等控制字符")
	}
	if strings.TrimSpace(t.Title) == "" {
		bad("标题不能为空")
	}
	if utf8.RuneCountInString(t.Title) > MaxTitleRunes {
		bad("标题最长 %d 个字符", MaxTitleRunes)
	}
	if badCtl(t.Title, false) {
		bad("标题里不能有换行等控制字符")
	}
	if utf8.RuneCountInString(t.Icon) > MaxIconRunes || badCtl(t.Icon, false) {
		bad("图标最长 %d 个字符", MaxIconRunes)
	}
	if !validTone(t.Tone) {
		bad("状态色只能是 info / ok / warn / crit")
	}
	if len(t.Blocks) > MaxBlocks {
		bad("内容块最多 %d 个", MaxBlocks)
	}

	seen := map[string]bool{}
	refs := func(where, s string, allowNL bool, maxRunes int) {
		if utf8.RuneCountInString(s) > maxRunes {
			bad("%s 最长 %d 个字符", where, maxRunes)
		}
		if badCtl(s, allowNL) {
			bad("%s 里不能有控制字符", where)
		}
		names, err := Refs(s)
		if err != nil {
			bad("%s：%v", where, err)
			return
		}
		for _, n := range names {
			seen[n] = true
		}
	}
	refs("标题", t.Title, false, MaxTitleRunes)

	for i, b := range t.Blocks {
		pos := fmt.Sprintf("第 %d 块", i+1)
		switch b.T {
		case BlockFields:
			if len(b.Rows) == 0 {
				bad("%s（字段表）至少要有一行", pos)
			}
			if len(b.Rows) > MaxRows {
				bad("%s（字段表）最多 %d 行", pos, MaxRows)
			}
			for j, r := range b.Rows {
				if strings.TrimSpace(r.L) == "" {
					bad("%s 第 %d 行的名称不能为空", pos, j+1)
				}
				refs(fmt.Sprintf("%s 第 %d 行的名称", pos, j+1), r.L, false, MaxLabelRunes)
				refs(fmt.Sprintf("%s 第 %d 行的值", pos, j+1), r.V, true, MaxValueRunes)
			}
		case BlockText:
			if strings.TrimSpace(b.V) == "" {
				bad("%s（文本）内容不能为空", pos)
			}
			refs(fmt.Sprintf("第 %d 块（文本）", i+1), b.V, true, MaxTextRunes)
		case BlockCode:
			if strings.TrimSpace(b.V) == "" {
				bad("%s（SQL / 代码块）内容不能为空", pos)
			}
			refs(fmt.Sprintf("第 %d 块（SQL / 代码块）的小标题", i+1), b.L, false, MaxLabelRunes)
			refs(fmt.Sprintf("第 %d 块（SQL / 代码块）的内容", i+1), b.V, true, MaxValueRunes)
			switch b.Mode {
			case ModeHead, ModeMask, ModeFull, ModeNone:
			default:
				bad("%s（SQL / 代码块）的显示方式不合法", pos)
			}
			if (b.Mode == ModeHead || b.Mode == ModeMask) && (b.N < MinCodeN || b.N > MaxCodeN) {
				bad("%s（SQL / 代码块）的字数要在 %d–%d 之间", pos, MinCodeN, MaxCodeN)
			}
		case BlockLink:
			if strings.TrimSpace(b.L) == "" || strings.TrimSpace(b.V) == "" {
				bad("%s（链接）的文字和地址都不能为空", pos)
			}
			if b.As != "button" && b.As != "text" {
				bad("%s（链接）的样式只能是 button / text", pos)
			}
			refs(fmt.Sprintf("第 %d 块（链接）的文字", i+1), b.L, false, MaxLabelRunes)
			refs(fmt.Sprintf("第 %d 块（链接）的地址", i+1), b.V, false, MaxValueRunes)
			if lit := strings.TrimSpace(b.V); lit != "" && !strings.Contains(lit, "{{") {
				if u, err := url.Parse(lit); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
					bad("%s（链接）的地址必须是变量（如 {{url}}）或 http(s) 地址", pos)
				}
			}
		case BlockDivider:
		default:
			bad("%s 的类型不认识：%s", pos, strconv.Quote(b.T))
		}
	}

	// 变量
	for n := range seen {
		if _, ok := varByKey[n]; !ok {
			bad("用了不存在的变量 {{%s}}，请从变量面板里选", n)
			continue
		}
		if def != nil && !def.Has(n) {
			warn("这条消息里没有变量 {{%s}}（%s），发出去会是空的", n, varByKey[n].Name)
		}
	}
	if b, _ := marshalBlocks(t.Blocks); len(b) > MaxTplJSON {
		bad("模板太大（超过 %d KB），请精简", MaxTplJSON>>10)
	}
	return out
}

// MarshalBlocks 把内容块序列化成存库用的 JSON。
func MarshalBlocks(bs []Block) ([]byte, error) {
	if bs == nil {
		bs = []Block{}
	}
	return json.Marshal(bs)
}

func marshalBlocks(bs []Block) ([]byte, error) { return MarshalBlocks(bs) }

// UnmarshalBlocks 解析存库的内容块 JSON；空串得到空列表。
func UnmarshalBlocks(raw string) ([]Block, error) {
	if strings.TrimSpace(raw) == "" {
		return []Block{}, nil
	}
	var bs []Block
	if err := json.Unmarshal([]byte(raw), &bs); err != nil {
		return nil, err
	}
	if bs == nil {
		bs = []Block{}
	}
	return bs, nil
}

// HasCrit 判断问题里有没有不能保存的。
func HasCrit(is []Issue) bool {
	for _, i := range is {
		if i.Level == "crit" {
			return true
		}
	}
	return false
}
