// @author: kevinkelly
// @license: Apache Licence
// @file: execreport.go
// @time: 2026/10/28

package sqlflow

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"dbtool/internal/review"
)

// 执行报表：工单执行结束的通知里，除了结束时间 / 耗时 / 影响行数，再带上“汇总 + 执行记录”：
//
//	总数: 10 | 成功: 10 | 失败: 0 | 影响行数: 0
//	ID: 1 | 状态: ✅ Execute Successfully | 影响行数: 0
//	ID: 2 | 状态: ❌ Execute failed | 影响行数: 0 | 错误: …
//
// 非批量工单每条语句一行；批量工单（多个实例）只给“成功多少个实例、失败多少个实例”，不列出具体的实例名，也没有逐条记录。
// 影响行数小于 0（SQL Server 的 DDL 返回 -1）按 0 算。
// 记录太长时只显示前面一部分（失败 / 未执行的优先保留），超过 1000 字不全部显示，末尾用 ... 结尾。

// 一条语句（或一个实例）的执行结果。
const (
	outOK = iota
	outFail
	outSkip
)

const (
	maxErrRunes  = 100  // 记录里每条错误信息的字数上限
	execLogLimit = 1000 // 执行记录的字数上限，放不下时末尾用 ...
)

// execReport 是整理后的执行报表。
type execReport struct {
	Total    int    // 语句数；批量工单是实例数
	OK       int    // 成功
	Fail     int    // 失败
	Skip     int    // 未执行（前序语句失败被跳过 / 批量实例已关闭）
	Affected int64  // 影响行数合计（小于 0 的按 0 算）
	Batch    bool   // 批量工单：Total / OK / Fail / Skip 是实例个数，没有逐条记录
	Log      string // 执行记录（多行文本，已按长度截断）；批量工单为空
}

// Summary 是一行汇总文字。批量工单只写个数，不写实例名。
func (r execReport) Summary() string {
	if r.Batch {
		s := fmt.Sprintf("共 %d 个实例 | 成功 %d 个实例 | 失败 %d 个实例", r.Total, r.OK, r.Fail) // i18n-ignore 通知正文（语言属于接收方）
		if r.Skip > 0 {
			s += fmt.Sprintf(" | 未执行 %d 个实例", r.Skip) // i18n-ignore 通知正文（语言属于接收方）
		}
		return s + fmt.Sprintf(" | 影响行数: %d", r.Affected) // i18n-ignore 通知正文（语言属于接收方）
	}
	s := fmt.Sprintf("总数: %d | 成功: %d | 失败: %d", r.Total, r.OK, r.Fail) // i18n-ignore 通知正文（语言属于接收方）
	if r.Skip > 0 {
		s += fmt.Sprintf(" | 未执行: %d", r.Skip) // i18n-ignore 通知正文（语言属于接收方）
	}
	return s + fmt.Sprintf(" | 影响行数: %d", r.Affected) // i18n-ignore 通知正文（语言属于接收方）
}

func rowOutcome(r review.Row) int {
	st := r.StageStatus
	switch {
	case strings.Contains(st, "Execute Successfully"):
		return outOK
	case r.BatchClosed, r.Stage == "SKIPPED", r.Stage == "CHECKED", st == "Audit completed", st == "未执行": // i18n-ignore 与存库的结果状态文本做匹配
		return outSkip
	case r.ErrLevel >= review.LevelWarning:
		return outFail
	}
	return outOK
}

func nonNeg(n review.Flex) int64 {
	if n < 0 {
		return 0
	}
	return int64(n)
}

// cutErr 把错误信息压成一行并限制字数。
func cutErr(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxErrRunes {
		return string(r[:maxErrRunes]) + "..."
	}
	return s
}

// reportLine 是一行记录；out 决定被截断时的取舍优先级。
type reportLine struct {
	text string
	out  int
}

// buildExecReport 把工单的执行结果行整理成执行报表。没有可用的结果行时返回 ok=false。
func buildExecReport(rows []review.Row) (rep execReport, ok bool) {
	var stmts []review.Row
	batch := false
	for _, r := range rows {
		if r.IsSummary {
			continue
		}
		if r.InstanceName != "" {
			batch = true
		}
		stmts = append(stmts, r)
	}
	if len(stmts) == 0 {
		return rep, false
	}
	rep.Batch = batch
	var lines []reportLine
	if batch {
		// 批量工单：只统计成功 / 失败 / 未执行的实例个数，不列出实例名，也不逐条列语句。
		type inst struct {
			affected       int64
			ok, fail, skip int
			closed         bool
		}
		by := map[string]*inst{}
		var order []string
		for _, r := range stmts {
			in := by[r.InstanceName]
			if in == nil {
				in = &inst{}
				by[r.InstanceName] = in
				order = append(order, r.InstanceName)
			}
			in.affected += nonNeg(r.AffectedRows)
			in.closed = in.closed || r.BatchClosed
			switch rowOutcome(r) {
			case outFail:
				in.fail++
			case outSkip:
				in.skip++
			default:
				in.ok++
			}
		}
		for _, name := range order {
			in := by[name]
			rep.Total++
			rep.Affected += in.affected
			switch {
			case in.fail > 0:
				rep.Fail++
			case in.closed || in.ok == 0:
				rep.Skip++
			default:
				rep.OK++
			}
		}
		return rep, true
	} else {
		for i, r := range stmts {
			id := int64(r.ID)
			if id <= 0 {
				id = int64(i + 1)
			}
			rep.Total++
			aff := nonNeg(r.AffectedRows)
			st := strings.TrimSpace(r.StageStatus)
			switch rowOutcome(r) {
			case outFail:
				rep.Fail++
				if st == "" {
					st = "Execute failed"
				}
				l := fmt.Sprintf("ID: %d | 状态: ❌ %s | 影响行数: %d", id, st, aff) // i18n-ignore 通知正文（语言属于接收方）
				if e := cutErr(r.ErrorMessage); e != "" && e != "None" {
					l += " | 错误: " + e // i18n-ignore 通知正文（语言属于接收方）
				}
				rep.Affected += aff
				lines = append(lines, reportLine{l, outFail})
			case outSkip:
				rep.Skip++
				lines = append(lines, reportLine{fmt.Sprintf("ID: %d | 状态: ⏭ 未执行", id), outSkip}) // i18n-ignore 通知正文（语言属于接收方）
			default:
				rep.OK++
				if st == "" {
					st = "Execute Successfully"
				}
				rep.Affected += aff
				lines = append(lines, reportLine{fmt.Sprintf("ID: %d | 状态: ✅ %s | 影响行数: %d", id, st, aff), outOK}) // i18n-ignore 通知正文（语言属于接收方）
			}
		}
	}
	rep.Log = joinReportLines(lines, execLogLimit)
	return rep, true
}

// joinReportLines 把记录拼成文本：全部放得下就按原顺序全放；放不下时优先保留失败、未执行的，再补成功的，
// 仍按原顺序显示，末尾用 ... 结尾（总字数不超过 limit）。
func joinReportLines(lines []reportLine, limit int) string {
	width := func(s string) int { return utf8.RuneCountInString(s) + 1 }
	total := 0
	for _, l := range lines {
		total += width(l.text)
	}
	if total-1 <= limit {
		out := make([]string, len(lines))
		for i, l := range lines {
			out[i] = l.text
		}
		return strings.Join(out, "\n")
	}
	idx := make([]int, len(lines))
	for i := range idx {
		idx[i] = i
	}
	// 优先级：失败 > 未执行 > 成功（同级保持原顺序）
	prio := func(o int) int { return map[int]int{outFail: 0, outSkip: 1, outOK: 2}[o] }
	sort.SliceStable(idx, func(a, b int) bool { return prio(lines[idx[a]].out) < prio(lines[idx[b]].out) })
	budget := limit - len("\n...")
	used := 0
	pick := map[int]bool{}
	for _, i := range idx {
		w := width(lines[i].text)
		if used+w > budget {
			break
		}
		used += w
		pick[i] = true
	}
	var out []string
	for i, l := range lines {
		if pick[i] {
			out = append(out, l.text)
		}
	}
	out = append(out, "...")
	return strings.Join(out, "\n")
}

// reportVars 把执行报表放进消息变量（模板里用 {{exec_summary}} / {{exec_log}} 等）。
func reportVars(v map[string]string, rep execReport) {
	v["total_count"] = strconv.Itoa(rep.Total)
	v["success_count"] = strconv.Itoa(rep.OK)
	v["fail_count"] = strconv.Itoa(rep.Fail)
	v["exec_summary"] = rep.Summary()
	v["exec_log"] = rep.Log
}
