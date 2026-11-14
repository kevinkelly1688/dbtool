// @author: kevinkelly
// @license: Apache Licence
// @file: diff.go
// @time: 2026/10/28

package giconfig

import "fmt"

// DiffLine 是对比结果里的一行。Op：" " 未变、"+" 新增、"-" 删除、"~" 省略了若干未变的行。
type DiffLine struct {
	Op   string `json:"op"`
	Text string `json:"text"`
	Old  int    `json:"old,omitempty"` // 在旧文件里的行号（从 1 开始）
	New  int    `json:"new,omitempty"`
}

// DiffResult 是行级对比结果，前端按 pre.diff 的样式渲染。
type DiffResult struct {
	Lines   []DiffLine `json:"lines"`
	Added   int        `json:"added"`
	Removed int        `json:"removed"`
	Same    bool       `json:"same"`
}

// maxDiffCells 限制最长公共子序列的计算规模（行数乘积），超出则退化为“整段替换”。
const maxDiffCells = 4_000_000

// maskPair 给两份文件的密码类参数脱敏：值相同则两边都显示占位符；不同则新文件一侧标注“已修改”，
// 这样对比里能看出密码变过，又不会泄露内容。
func maskPair(oldSrc, newSrc string) (string, string) {
	ol := splitLines(normalizeNewlines(oldSrc))
	nl := splitLines(normalizeNewlines(newSrc))
	oe, _ := parseEntries(ol)
	ne, _ := parseEntries(nl)
	find := func(es []entry, e entry) *entry {
		for i := range es {
			if es[i].Section == e.Section && es[i].Key == e.Key {
				return &es[i]
			}
		}
		return nil
	}
	masked := `"` + MaskPlaceholder + `"`
	changed := `"` + MaskPlaceholder + `（已修改）"` // i18n-ignore 配置文件内容里的标记，不翻译
	for i := len(oe) - 1; i >= 0; i-- {
		if e := oe[i]; IsSensitiveKey(e.Key) && isStringRaw(e.Raw) && !isEmptyStringRaw(e.Raw) {
			ol = replaceRaw(ol, e, masked)
		}
	}
	for i := len(ne) - 1; i >= 0; i-- {
		e := ne[i]
		if !IsSensitiveKey(e.Key) || !isStringRaw(e.Raw) || isEmptyStringRaw(e.Raw) {
			continue
		}
		if o := find(oe, e); o != nil && o.Raw == e.Raw {
			nl = replaceRaw(nl, e, masked)
		} else {
			nl = replaceRaw(nl, e, changed)
		}
	}
	return joinLines(ol), joinLines(nl)
}

// Diff 比较两份配置文件（密码已脱敏）。context 是每处改动前后保留的未变行数，< 0 表示全部保留。
func Diff(oldSrc, newSrc string, context int) DiffResult {
	oldSrc, newSrc = maskPair(oldSrc, newSrc)
	a := splitLines(oldSrc)
	b := splitLines(newSrc)
	ops := diffOps(a, b)
	res := DiffResult{Lines: []DiffLine{}}
	oi, ni := 0, 0
	all := make([]DiffLine, 0, len(ops))
	for _, op := range ops {
		switch op {
		case '=':
			oi++
			ni++
			all = append(all, DiffLine{Op: " ", Text: a[oi-1], Old: oi, New: ni})
		case '-':
			oi++
			res.Removed++
			all = append(all, DiffLine{Op: "-", Text: a[oi-1], Old: oi})
		case '+':
			ni++
			res.Added++
			all = append(all, DiffLine{Op: "+", Text: b[ni-1], New: ni})
		}
	}
	res.Same = res.Added == 0 && res.Removed == 0
	if context < 0 {
		res.Lines = all
		return res
	}
	// 只保留改动及其前后 context 行
	keep := make([]bool, len(all))
	for i, l := range all {
		if l.Op == " " {
			continue
		}
		for j := i - context; j <= i+context; j++ {
			if j >= 0 && j < len(all) {
				keep[j] = true
			}
		}
	}
	skipped := 0
	flush := func() {
		if skipped > 0 {
			res.Lines = append(res.Lines, DiffLine{Op: "~", Text: fmt.Sprintf("… 省略 %d 行未变内容 …", skipped)})
			skipped = 0
		}
	}
	for i, l := range all {
		if keep[i] {
			flush()
			res.Lines = append(res.Lines, l)
		} else {
			skipped++
		}
	}
	flush()
	return res
}

// diffOps 返回编辑脚本：'=' 相同、'-' 删除（来自 a）、'+' 新增（来自 b）。
func diffOps(a, b []string) []byte {
	// 去掉相同的头尾，缩小需要比对的范围
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	am, bm := a[pre:len(a)-suf], b[pre:len(b)-suf]
	var ops []byte
	for i := 0; i < pre; i++ {
		ops = append(ops, '=')
	}
	n, m := len(am), len(bm)
	switch {
	case n == 0:
		for range bm {
			ops = append(ops, '+')
		}
	case m == 0:
		for range am {
			ops = append(ops, '-')
		}
	case (n+1)*(m+1) > maxDiffCells:
		for range am {
			ops = append(ops, '-')
		}
		for range bm {
			ops = append(ops, '+')
		}
	default:
		// lcs[i][j] = am[i:] 与 bm[j:] 的最长公共子序列长度
		w := m + 1
		lcs := make([]int32, (n+1)*w)
		for i := n - 1; i >= 0; i-- {
			for j := m - 1; j >= 0; j-- {
				if am[i] == bm[j] {
					lcs[i*w+j] = lcs[(i+1)*w+j+1] + 1
				} else if lcs[(i+1)*w+j] >= lcs[i*w+j+1] {
					lcs[i*w+j] = lcs[(i+1)*w+j]
				} else {
					lcs[i*w+j] = lcs[i*w+j+1]
				}
			}
		}
		i, j := 0, 0
		for i < n && j < m {
			switch {
			case am[i] == bm[j]:
				ops = append(ops, '=')
				i++
				j++
			case lcs[(i+1)*w+j] >= lcs[i*w+j+1]:
				ops = append(ops, '-')
				i++
			default:
				ops = append(ops, '+')
				j++
			}
		}
		for ; i < n; i++ {
			ops = append(ops, '-')
		}
		for ; j < m; j++ {
			ops = append(ops, '+')
		}
	}
	for i := 0; i < suf; i++ {
		ops = append(ops, '=')
	}
	return ops
}
