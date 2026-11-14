// @author: kevinkelly
// @license: Apache Licence
// @file: pkrange.go
// @time: 2026/10/28

package engine

import (
	"math/big"
	"strings"
)

// 会删数据的模式（归档并删除 / 仅清理）读取时不排序、没有断点：每批取走“当前符合条件的前 N 行”，删掉之后下一批自然取到剩下的。
// 但运行记录里要记下本次处理过的主键范围 [MinPK, MaxPK]（恢复数据时按它从备份表补回），不排序就只能逐批自己算最小 / 最大值，
// 所以比较规则必须和数据库一致：整数、小数、日期时间类的主键能在本地比对；字符串等受排序规则（大小写、重音）影响，本地比不准，
// 这类主键的表仍按主键排序读取（带断点），不走不排序读取。

// readUnordered 判断这次运行是否不排序读取：只有会删数据的模式，且主键各列都能在本地比大小。
func readUnordered(mode Mode, info TableInfo) bool {
	if !isDeleting(mode) {
		return false
	}
	_, ok := pkComparators(info)
	return ok
}

// pkComparators 返回主键各列的比较函数（按主键列顺序）；有一列不能在本地比大小就返回 ok=false。
func pkComparators(info TableInfo) ([]func(a, b string) int, bool) {
	if len(info.PrimaryKey) == 0 {
		return nil, false
	}
	out := make([]func(a, b string) int, len(info.PrimaryKey))
	for i, name := range info.PrimaryKey {
		var cmp func(a, b string) int
		for _, c := range info.Columns {
			if strings.EqualFold(c.Name, name) {
				cmp = colComparator(c.Type)
				break
			}
		}
		if cmp == nil {
			return nil, false
		}
		out[i] = cmp
	}
	return out, true
}

func colComparator(colType string) func(a, b string) int {
	t := strings.ToLower(strings.TrimSpace(colType))
	if i := strings.IndexAny(t, "( "); i >= 0 {
		t = t[:i]
	}
	switch t {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint", "year":
		return cmpInteger
	case "decimal", "numeric":
		return cmpDecimal
	case "date", "datetime", "timestamp":
		return strings.Compare // 定宽的 “2006-01-02 15:04:05[.小数]”，逐字符比较就是时间先后（小数位末尾的 0 已去掉，也成立）
	}
	return nil
}

func cmpInteger(a, b string) int {
	x, ok1 := new(big.Int).SetString(strings.TrimSpace(a), 10)
	y, ok2 := new(big.Int).SetString(strings.TrimSpace(b), 10)
	if !ok1 || !ok2 {
		return strings.Compare(a, b)
	}
	return x.Cmp(y)
}

func cmpDecimal(a, b string) int {
	x, ok1 := new(big.Rat).SetString(strings.TrimSpace(a))
	y, ok2 := new(big.Rat).SetString(strings.TrimSpace(b))
	if !ok1 || !ok2 {
		return strings.Compare(a, b)
	}
	return x.Cmp(y)
}

// pkRange 逐批记下处理过的最小 / 最大主键。
type pkRange struct {
	cmp      []func(a, b string) int
	min, max []string
}

func newPKRange(cmp []func(a, b string) int) *pkRange { return &pkRange{cmp: cmp} }

func (p *pkRange) compare(a, b []string) int {
	for i, c := range p.cmp {
		if i >= len(a) || i >= len(b) {
			break
		}
		if v := c(a[i], b[i]); v != 0 {
			return v
		}
	}
	return 0
}

func (p *pkRange) add(pks [][]string) {
	for _, pk := range pks {
		if len(pk) != len(p.cmp) {
			continue
		}
		if p.min == nil || p.compare(pk, p.min) < 0 {
			p.min = copyStrings(pk)
		}
		if p.max == nil || p.compare(pk, p.max) > 0 {
			p.max = copyStrings(pk)
		}
	}
}
