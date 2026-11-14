// @author: kevinkelly
// @license: Apache Licence
// @file: mapper.go
// @time: 2026/10/28

package sqlopt

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// MyBatis3 Mapper XML → SQL 列表。
//
// 只做静态展开，目的是得到能交给分析器的 SQL，不追求和 MyBatis 运行时完全一致：
//   - <if>、<when> 的内容都保留（假设条件成立），<choose> 只取第一个 <when>（没有则取 <otherwise>）；
//   - <where>/<set>/<trim> 按 MyBatis 规则补前后缀、去掉多余的 AND/OR/逗号；
//   - <foreach> 展开一次（带 open/close）；<include refid> 内联 <sql> 片段，支持 <property>；
//   - #{x} 换成 ?，${x} 换成名字 x（既可当标识符也可当字符串内容）。
// 解析用 encoding/xml，不会加载外部实体（没有 XXE 问题）；节点数、嵌套深度、展开长度都有上限。

const (
	maxXMLDepth   = 64
	maxXMLNodes   = 200000
	maxIncludeDep = 10
	maxRenderSize = 1 << 20
)

type xnode struct {
	name     string
	attrs    map[string]string
	children []*xnode
	text     string // name == "" 时为文本节点
}

var errMapperTooBig = errors.New("Mapper XML 过于复杂（节点过多或嵌套过深）")

// parseMapperTree 把 XML 解析成简单的树，返回根元素。
func parseMapperTree(text string) (*xnode, error) {
	d := xml.NewDecoder(strings.NewReader(text))
	d.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil } // 内容已经是 UTF-8，声明的编码不理会
	var stack []*xnode
	var root *xnode
	nodes := 0
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		nodes++
		if nodes > maxXMLNodes {
			return nil, errMapperTooBig
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if len(stack) >= maxXMLDepth {
				return nil, errMapperTooBig
			}
			n := &xnode{name: strings.ToLower(t.Name.Local), attrs: map[string]string{}}
			for _, a := range t.Attr {
				n.attrs[strings.ToLower(a.Name.Local)] = a.Value
			}
			if len(stack) == 0 {
				if root != nil {
					return nil, errors.New("XML 有多个根元素")
				}
				root = n
			} else {
				p := stack[len(stack)-1]
				p.children = append(p.children, n)
			}
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			if len(stack) > 0 {
				p := stack[len(stack)-1]
				p.children = append(p.children, &xnode{text: string(t)})
			}
		}
	}
	if root == nil {
		return nil, errors.New("没有 XML 内容")
	}
	return root, nil
}

type renderer struct {
	frags map[string]*xnode
	size  int // 当前语句已展开的长度
	steps int // 当前语句已访问的节点数（防 <include> 套娃放大）
	total int // 所有语句展开后的总长度
	over  bool
}

const (
	maxRenderSteps = 2000000
	maxRenderTotal = 32 << 20
)

var (
	reHash   = regexp.MustCompile(`#\{[^}]*\}`)
	reDollar = regexp.MustCompile(`\$\{\s*([^}]*?)\s*\}`)
	reSpaces = regexp.MustCompile(`\s+`)
	reName   = regexp.MustCompile(`[^A-Za-z0-9_]+`)
)

// parseMapper 从 Mapper XML 文本得到 SQL 列表。不是 <mapper> 根元素返回 (nil, errNotMapper)。
var errNotMapper = errors.New("不是 MyBatis Mapper XML")

func parseMapper(text string) ([]SQLRow, error) {
	root, err := parseMapperTree(text)
	if err != nil {
		return nil, err
	}
	if root.name != "mapper" {
		return nil, errNotMapper
	}
	r := &renderer{frags: map[string]*xnode{}}
	for _, c := range root.children {
		if c.name == "sql" && c.attrs["id"] != "" {
			r.frags[c.attrs["id"]] = c
		}
	}
	var rows []SQLRow
	seen := map[string]int{}
	for _, c := range root.children {
		switch c.name {
		case "select", "insert", "update", "delete":
		default:
			continue
		}
		r.size, r.steps, r.over = 0, 0, false
		sql := r.children(c, nil, 0, 0)
		sql = finishSQL(sql)
		if r.total += len(sql); r.total > maxRenderTotal {
			return nil, bad("Mapper XML 展开后过大（<include> 嵌套过多），请检查文件")
		}
		if sql == "" && !r.over {
			continue
		}
		id := c.attrs["id"]
		if id == "" {
			id = c.name
		}
		seen[id]++
		if seen[id] > 1 {
			id = id + "#" + itoa(seen[id])
		}
		row := SQLRow{ID: id, SQL: sql}
		if r.over {
			row.Err = "该语句展开后过长，已跳过"
		}
		rows = append(rows, row)
		if len(rows) > MaxStatements {
			return nil, bad(fmt.Sprintf("文件里的 SQL 超过 %d 条，请拆分后再分析", MaxStatements))
		}
	}
	return rows, nil
}

func finishSQL(s string) string {
	s = reHash.ReplaceAllString(s, "?")
	s = reDollar.ReplaceAllStringFunc(s, func(m string) string {
		name := reName.ReplaceAllString(reDollar.FindStringSubmatch(m)[1], "_")
		if name == "" {
			name = "param"
		}
		return name
	})
	return strings.TrimSpace(reSpaces.ReplaceAllString(s, " "))
}

// children 渲染一个节点的全部子节点。props 是 <include> 传入的属性（替换片段里的 ${name}）。
func (r *renderer) children(n *xnode, props map[string]string, depth, incl int) string {
	var b strings.Builder
	for _, c := range n.children {
		if r.over {
			break
		}
		b.WriteString(r.node(c, props, depth+1, incl))
	}
	return b.String()
}

func (r *renderer) node(n *xnode, props map[string]string, depth, incl int) string {
	if r.steps++; depth > maxXMLDepth || r.steps > maxRenderSteps {
		r.over = true
		return ""
	}
	if n.name == "" {
		s := n.text
		for k, v := range props {
			s = strings.ReplaceAll(s, "${"+k+"}", v)
		}
		r.size += len(s)
		if r.size > maxRenderSize {
			r.over = true
			return ""
		}
		return s
	}
	switch n.name {
	case "bind", "selectkey", "property", "sql":
		return ""
	case "choose":
		var other *xnode
		for _, c := range n.children {
			switch c.name {
			case "when":
				return " " + r.children(c, props, depth, incl) + " "
			case "otherwise":
				if other == nil {
					other = c
				}
			}
		}
		if other != nil {
			return " " + r.children(other, props, depth, incl) + " "
		}
		return ""
	case "where":
		s := strings.TrimSpace(r.children(n, props, depth, incl))
		s = stripWord(s, "and", "or")
		if s == "" {
			return ""
		}
		return " WHERE " + s + " "
	case "set":
		s := strings.Trim(strings.TrimSpace(r.children(n, props, depth, incl)), ", \t\r\n")
		if s == "" {
			return ""
		}
		return " SET " + s + " "
	case "trim":
		s := strings.TrimSpace(r.children(n, props, depth, incl))
		if s == "" {
			return ""
		}
		for _, o := range strings.Split(n.attrs["prefixoverrides"], "|") {
			if o = strings.TrimSpace(o); o != "" && len(s) >= len(o) && strings.EqualFold(s[:len(o)], o) {
				s = strings.TrimSpace(s[len(o):])
				break
			}
		}
		for _, o := range strings.Split(n.attrs["suffixoverrides"], "|") {
			if o = strings.TrimSpace(o); o != "" && len(s) >= len(o) && strings.EqualFold(s[len(s)-len(o):], o) {
				s = strings.TrimSpace(s[:len(s)-len(o)])
				break
			}
		}
		return " " + n.attrs["prefix"] + " " + s + " " + n.attrs["suffix"] + " "
	case "foreach":
		return " " + n.attrs["open"] + r.children(n, props, depth, incl) + n.attrs["close"] + " "
	case "include":
		if incl >= maxIncludeDep {
			r.over = true
			return ""
		}
		ref := n.attrs["refid"]
		if i := strings.LastIndex(ref, "."); i >= 0 {
			ref = ref[i+1:]
		}
		frag := r.frags[ref]
		if frag == nil {
			return ""
		}
		np := map[string]string{}
		for k, v := range props {
			np[k] = v
		}
		for _, c := range n.children {
			if c.name == "property" {
				np[c.attrs["name"]] = c.attrs["value"]
			}
		}
		return " " + r.children(frag, np, depth, incl+1) + " "
	}
	// if / when / otherwise / 未知标签：保留内容
	return " " + r.children(n, props, depth, incl) + " "
}

// stripWord 去掉开头的某个关键字（按单词边界，不区分大小写）。
func stripWord(s string, words ...string) string {
	for _, w := range words {
		if len(s) >= len(w) && strings.EqualFold(s[:len(w)], w) && (len(s) == len(w) || !isWordByte(s[len(w)])) {
			return strings.TrimSpace(s[len(w):])
		}
	}
	return s
}

func isWordByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}
