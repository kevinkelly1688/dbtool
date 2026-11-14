// @author: kevinkelly
// @license: Apache Licence
// @file: tpl_render.go
// @time: 2026/10/28

package notify

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

// 本文件：把模板 + 事件渲染成与渠道无关的 Message（中间结构）；各渠道的原生载荷见 tpl_channels.go。

// Message 是渲染好的一条消息。
type Message struct {
	Key    string   `json:"key"`   // 消息键，如 sql.finished
	Scope  string   `json:"scope"` // 命中的模板范围：global / c:<渠道 ID>
	Title  string   `json:"title"`
	Icon   string   `json:"icon,omitempty"`
	Tone   string   `json:"tone"`
	Blocks []MBlock `json:"blocks"`
}

// MBlock 是渲染好的内容块（变量已展开、空块已去掉）。
type MBlock struct {
	T    string `json:"t"`
	Rows []Row  `json:"rows,omitempty"`
	L    string `json:"l,omitempty"`
	V    string `json:"v,omitempty"`
	As   string `json:"as,omitempty"`
	Cut  bool   `json:"cut,omitempty"` // 代码块被截断过
}

// Head 是“图标 + 标题”。
func (m *Message) Head() string { return strings.TrimSpace(m.Icon + " " + m.Title) }

func (m *Message) clone() *Message {
	c := *m
	c.Blocks = make([]MBlock, len(m.Blocks))
	for i, b := range m.Blocks {
		c.Blocks[i] = b
		c.Blocks[i].Rows = append([]Row(nil), b.Rows...)
	}
	return &c
}

// 单个变量值进入消息前的字数上限。
const (
	maxVarRunes = 5000
	maxRowRunes = 1500
	maxTxtRunes = 3000
)

// eventValues 取出事件里可供模板用的变量值：产生方填的 Vars（只留这条消息声明过的）加上系统自带的几个。
func eventValues(def *MessageDef, ev Event) map[string]string {
	vals := map[string]string{}
	put := func(k, v string) {
		if def != nil && !def.Has(k) {
			return
		}
		if _, ok := varByKey[k]; !ok {
			return
		}
		limit := maxVarRunes
		if sqlish(k) {
			limit = maxSQLVar
		}
		if utf8.RuneCountInString(v) > limit {
			v = string([]rune(v)[:limit])
		}
		vals[k] = v
	}
	for k, v := range ev.Vars {
		put(k, v)
	}
	put("msg_title", ev.Title)
	put("msg_content", ev.Content)
	if _, ok := vals["applicant_user"]; !ok {
		put("applicant_user", ev.Applicant)
	}
	if def != nil {
		put("event_label", def.Name)
	}
	return vals
}

// Render 用模板渲染事件。模板里写错（理论上保存时已校验）返回错误，调用方沿用系统默认文案。
func (t Template) Render(def *MessageDef, ev Event) (*Message, error) {
	vals := eventValues(def, ev)
	// 代码块以外的位置：{{sql}} 隐藏常量并截短，避免整段 SQL 被不小心推出去。
	safe := func(k string) string {
		if sqlish(k) {
			return inlineSQL(vals[k])
		}
		return vals[k]
	}
	raw := func(k string) string { return vals[k] }

	m := &Message{Scope: t.Scope, Tone: t.Tone, Icon: oneLine(t.Icon)}
	if def != nil {
		m.Key = def.Key
	}
	if !validTone(m.Tone) {
		m.Tone = ToneInfo
	}
	title, err := Expand(t.Title, safe)
	if err != nil {
		return nil, fmt.Errorf("标题：%w", err)
	}
	if m.Title = oneLine(title); m.Title == "" {
		m.Title = oneLine(ev.Title)
	}
	m.Title = cutRunesEllipsis(m.Title, MaxTitleRunes)

	for i, b := range t.Blocks {
		where := fmt.Sprintf("第 %d 块", i+1)
		switch b.T {
		case BlockFields:
			var rows []Row
			for _, r := range b.Rows {
				l, err := Expand(r.L, safe)
				if err != nil {
					return nil, fmt.Errorf("%s：%w", where, err)
				}
				v, err := Expand(r.V, safe)
				if err != nil {
					return nil, fmt.Errorf("%s：%w", where, err)
				}
				l, v = oneLine(l), multiLine(v)
				if l == "" || v == "" {
					continue // 取不到值的行不发（空字段在 Slack / 飞书里会被拒绝）
				}
				rows = append(rows, Row{L: cutRunesEllipsis(l, MaxLabelRunes*2), V: cutRunesEllipsis(v, maxRowRunes), Short: r.Short})
			}
			if len(rows) > 0 {
				m.Blocks = append(m.Blocks, MBlock{T: BlockFields, Rows: rows})
			}
		case BlockText:
			v, err := Expand(b.V, safe)
			if err != nil {
				return nil, fmt.Errorf("%s：%w", where, err)
			}
			if v = multiLine(v); v != "" {
				m.Blocks = append(m.Blocks, MBlock{T: BlockText, V: cutRunesEllipsis(v, maxTxtRunes)})
			}
		case BlockCode:
			l, err := Expand(b.L, safe)
			if err != nil {
				return nil, fmt.Errorf("%s：%w", where, err)
			}
			v, err := Expand(b.V, raw)
			if err != nil {
				return nil, fmt.Errorf("%s：%w", where, err)
			}
			if strings.TrimSpace(v) == "" {
				continue
			}
			text, cut := SQLDisplay(v, b.Mode, b.N)
			m.Blocks = append(m.Blocks, MBlock{T: BlockCode, L: oneLine(l), V: text, Cut: cut})
		case BlockLink:
			l, err := Expand(b.L, safe)
			if err != nil {
				return nil, fmt.Errorf("%s：%w", where, err)
			}
			u, err := Expand(b.V, safe)
			if err != nil {
				return nil, fmt.Errorf("%s：%w", where, err)
			}
			l, u = oneLine(l), strings.TrimSpace(oneLine(u))
			if l == "" || !httpURL(u) {
				continue
			}
			as := b.As
			if as != "text" {
				as = "button"
			}
			m.Blocks = append(m.Blocks, MBlock{T: BlockLink, L: cutRunesEllipsis(l, 60), V: u, As: as})
		case BlockDivider:
			m.Blocks = append(m.Blocks, MBlock{T: BlockDivider})
		}
	}
	return m, nil
}

// httpURL 判断是不是可以放进消息的 http(s) 地址。
func httpURL(s string) bool {
	if s == "" || len(s) > 2000 {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// Text 返回消息的纯文本形式（邮件的 text/plain 部分、纯文本渠道、预览里的“文字版”用）。
func (m *Message) Text() string {
	var b strings.Builder
	b.WriteString(m.Head())
	b.WriteString("\n")
	for _, bl := range m.Blocks {
		b.WriteString("\n")
		switch bl.T {
		case BlockFields:
			for i, r := range bl.Rows {
				if i > 0 {
					b.WriteString("\n")
				}
				b.WriteString(r.L + "：" + r.V)
			}
		case BlockText:
			b.WriteString(bl.V)
		case BlockCode:
			if bl.L != "" {
				b.WriteString(bl.L + "：\n")
			}
			b.WriteString(bl.V)
		case BlockLink:
			b.WriteString(bl.L + "：" + bl.V)
		case BlockDivider:
			b.WriteString("----------")
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// shrink 把最长的代码块 / 文本缩短一半，供“消息超过渠道大小上限”时重试；没有可缩的返回 false。
func (m *Message) shrink() bool {
	best, bestN := -1, 0
	for i, b := range m.Blocks {
		var n int
		switch b.T {
		case BlockCode, BlockText:
			n = utf8.RuneCountInString(b.V)
		case BlockFields:
			for _, r := range b.Rows {
				if c := utf8.RuneCountInString(r.V); c > n {
					n = c
				}
			}
		}
		if n > bestN {
			best, bestN = i, n
		}
	}
	if best < 0 || bestN <= 200 {
		return false
	}
	half := bestN / 2
	b := &m.Blocks[best]
	switch b.T {
	case BlockCode, BlockText:
		b.V = strings.TrimRight(string([]rune(b.V)[:half]), " \t\n") + "\n…（消息过长，已截断）" // i18n-ignore 通知正文 / 模板内容
		b.Cut = true
	case BlockFields:
		for i, r := range b.Rows {
			if utf8.RuneCountInString(r.V) > half {
				b.Rows[i].V = string([]rune(r.V)[:half]) + "…"
			}
		}
	}
	return true
}
