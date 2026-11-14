// @author: kevinkelly
// @license: Apache Licence
// @file: trace.go
// @time: 2026/10/28

package partman

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 运行输出（trace）：对应原外部程序 logs/<env>_<db>_<table>.log 与 logs/push.log 的内容 —— 起止行、每项检查 / 决策与原因、
// 每条语句的执行结果与耗时、推送结果。随每次落盘的维护写进 dbt_partman_log.exec_result（不改表结构）：
// 数组里除了语句项，再加一个 kind="trace" 的项，其 trace 字段是 []traceLine。
// 原有读取方只认 sql_text / status / error，trace 项没有这些字段的有效值，不影响它们（见 summarizeLog / parseLog）。

const (
	traceKind = "trace"

	// 一条记录里 trace 的上限；exec_result 是 TEXT（64KB），整条记录再留出语句项的空间。
	maxTraceLines   = 300
	maxTraceBytes   = 40 * 1024
	maxLogBytes     = 60 * 1024 // 整个 exec_result 的上限
	maxTraceMsgRune = 500       // 单行消息上限
	maxLogSQLRune   = 2000      // 日志里单条语句文本上限
	traceTimeLayout = "2006-01-02 15:04:05.000"
)

// traceLine 是运行输出的一行。
type traceLine struct {
	T string `json:"time"`             // 毫秒精度时间 2006-01-02 15:04:05.000
	L string `json:"level"`            // INFO / WARN / ERROR
	M string `json:"message"`          // 消息（中文，措辞沿用原程序）
	C int64  `json:"cnf_id,omitempty"` // 只在“定时轮次”记录里：这一行属于哪个配置（cnf_id），按可见范围过滤用
}

// OutputView 是接口里的“运行输出”。Truncated 为 true 表示超出上限被截断（第二行有“…已截断 N 行”标记）。
type OutputView struct {
	Lines     []traceLine `json:"lines"`
	Truncated bool        `json:"truncated"`
}

func outputView(lines []traceLine) *OutputView {
	if lines == nil {
		lines = []traceLine{}
	}
	o := &OutputView{Lines: lines}
	for _, l := range lines {
		if strings.HasPrefix(l.M, "…已截断") { // i18n-ignore：识别 capTrace 写进去的截断标记（存的是中文原文）
			o.Truncated = true
		}
	}
	return o
}

// tracer 收集一次维护的运行输出。
type tracer struct {
	now   func() time.Time
	lines []traceLine
}

func newTracer(now func() time.Time) *tracer { return &tracer{now: now} }

func clipMsg(s string, n int) string {
	s = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func (t *tracer) addAt(at time.Time, level string, cnf int64, format string, a ...any) {
	msg := format
	if len(a) > 0 {
		msg = fmt.Sprintf(format, a...)
	}
	t.lines = append(t.lines, traceLine{T: at.Format(traceTimeLayout), L: level, M: clipMsg(msg, maxTraceMsgRune), C: cnf})
}

func (t *tracer) add(level, format string, a ...any) { t.addAt(t.now(), level, 0, format, a...) }

func (t *tracer) addCnf(level string, cnf int64, format string, a ...any) {
	t.addAt(t.now(), level, cnf, format, a...)
}

// addPlan 把计划计算过程中的检查 / 决策并入输出。
func (t *tracer) addPlan(p *Plan) {
	for _, l := range p.Log {
		t.add(l.Level, "%s", l.Msg)
	}
	for _, w := range p.Warnings {
		t.add("WARN", "%s", w)
	}
}

// capTrace 把输出限制在 maxLines 行 / maxBytes 字节内：先从最旧的 INFO 删起（第一行起始行保留），
// 仍然超限再删最旧的其它行；有删除就在第二行放“…已截断 N 行”标记（标记本身计入上限）。
func capTrace(lines []traceLine, maxLines, maxBytes int) []traceLine {
	size := func(ls []traceLine) int {
		n := 2
		for _, l := range ls {
			b, _ := json.Marshal(l)
			n += len(b) + 1
		}
		return n
	}
	if len(lines) <= maxLines && size(lines) <= maxBytes {
		return lines
	}
	out := append([]traceLine(nil), lines...)
	dropped := 0
	over := func() bool {
		return len(out)+1 > maxLines || size(out)+160 > maxBytes
	}
	remove := func(pred func(l traceLine) bool) {
		for i := 1; i < len(out) && over(); {
			if pred(out[i]) {
				out = append(out[:i], out[i+1:]...)
				dropped++
				continue
			}
			i++
		}
	}
	remove(func(l traceLine) bool { return l.L == "INFO" })
	remove(func(l traceLine) bool { return l.L == "WARN" })
	remove(func(l traceLine) bool { return true })
	if dropped > 0 {
		at := ""
		if len(out) > 0 {
			at = out[0].T
		}
		mark := traceLine{T: at, L: "WARN", M: fmt.Sprintf("…已截断 %d 行（运行输出超出 %d 行 / %dKB 上限，先删较早的 INFO）", dropped, maxTraceLines, maxTraceBytes/1024)}
		if len(out) == 0 {
			return []traceLine{mark}
		}
		out = append(out[:1], append([]traceLine{mark}, out[1:]...)...)
	}
	return out
}

// buildLogBody 序列化 exec_result：语句项 + trace 项，整体不超过 maxLogBytes（超了就先压 trace，再合并语句项），保证写库不会失败。
func buildLogBody(items []logItem, trace []traceLine, head logItem) string {
	for i := range items {
		items[i].SQLText = clipMsg(items[i].SQLText, maxLogSQLRune)
		items[i].Error = clipMsg(items[i].Error, 1000)
	}
	mk := func(its []logItem, tr []traceLine) string {
		all := append([]logItem(nil), its...)
		if len(tr) > 0 {
			ti := head
			ti.Kind, ti.Trace = traceKind, tr
			all = append(all, ti)
		}
		b, _ := json.Marshal(all)
		return string(b)
	}
	trace = capTrace(trace, maxTraceLines, maxTraceBytes)
	body := mk(items, trace)
	for limit := maxTraceBytes / 2; len(body) > maxLogBytes && limit >= 2048; limit /= 2 {
		trace = capTrace(trace, maxTraceLines, limit)
		body = mk(items, trace)
	}
	if len(body) > maxLogBytes && len(items) > 20 {
		keep := items[:20]
		okN, failN, skipN := 0, 0, 0
		for _, it := range items[20:] {
			switch it.Status {
			case "success":
				okN++
			case "failed":
				failN++
			default:
				skipN++
			}
		}
		status := "success"
		if failN > 0 {
			status = "failed"
		}
		keep = append(keep, logItem{SQLText: fmt.Sprintf("…另有 %d 条语句已省略（成功 %d / 失败 %d / 跳过 %d）", len(items)-20, okN, failN, skipN),
			Status: status, Operator: head.Operator, Source: head.Source, ExecAt: head.ExecAt})
		body = mk(keep, trace)
	}
	if len(body) > maxLogBytes && head.Round != nil && len(head.Round.Items) > 30 { // 轮次摘要太长：留 30 项
		r := *head.Round
		r.ItemsCut += len(r.Items) - 30
		r.Items = r.Items[:30]
		head.Round = &r
		body = mk(items, trace)
	}
	if len(body) > maxLogBytes { // 兜底：只留一条说明，绝不让插入失败
		trace = capTrace(trace, 20, 4096)
		body = mk(items[:minInt(len(items), 5)], trace)
	}
	return body
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// renderOutput 把一条日志的运行输出渲染成纯文本（导出 / 下载用），每行 “时间 [级别] 消息”。
func renderOutput(lines []traceLine) string {
	var b strings.Builder
	for _, l := range lines {
		fmt.Fprintf(&b, "%s [%-5s] %s\n", l.T, l.L, l.M)
	}
	return b.String()
}

// extractTrace 从 exec_result 里取出 trace 项。没有（旧日志、外部程序的日志）返回 nil。
func extractTrace(raw string) []traceLine {
	var arr []struct {
		Kind  string      `json:"kind"`
		Trace []traceLine `json:"trace"`
	}
	if json.Unmarshal([]byte(raw), &arr) != nil {
		return nil
	}
	var out []traceLine
	for _, it := range arr {
		if it.Kind == traceKind {
			out = append(out, it.Trace...)
		}
	}
	return out
}

// oneLine 把多行文本压成单行（多个空白合并），用于日志里展示语句。
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
