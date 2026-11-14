// @author: kevinkelly
// @license: Apache Licence
// @file: eval.go
// @time: 2026/10/28

package firewall

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	errNoTables  = errors.New("没有可用的库表解析（未提供语法树 / 词法解析）")
	errNoExplain = errors.New("没有接入 EXPLAIN 估算")
)

// tri 是条件的三值结果：不命中 / 命中 / 未知（事实取不到）。
type tri int

const (
	no tri = iota
	yes
	unknown
)

// featureHit 是结构特征命中时的说明（整句，便于按目录翻译）；内容 = "语句" + features[k]。
var featureHit = map[string]string{
	FeatNoWhere: "语句没有 WHERE", FeatNoLimit: "语句没有 LIMIT", FeatCartesian: "语句笛卡尔积", FeatMultiStmt: "语句一次提交多条语句",
	FeatSelectStar: "语句SELECT *", FeatLeadingWildcard: "语句LIKE 以 % 开头", FeatBigIn: "语句IN 列表过长",
}

// objPattern 是已拆开的对象模式。
type objPattern struct{ db, table, col string }

// 条件的求值成本：先便宜的（词法），再要库表解析的，最后要 EXPLAIN 的（设计 §5.2）。
const (
	costLexical = iota
	costObjects
	costExplain
)

// compiledCond 是已编译的条件。
type compiledCond struct {
	t        string
	cost     int
	stmts    map[string]bool
	feature  string
	phrases  []string
	re       *regexp.Regexp
	reSrc    string
	patterns []objPattern
	n        int64
	timeKind string
	fp       string
	label    string // 给命中说明用的条件名
}

// compiledRule 是已编译的规则：集合已建、正则已编译，求值时只做匹配。
type compiledRule struct {
	Rule
	ref       RuleRef
	conds     []compiledCond
	sceneSet  map[string]bool
	instSet   map[uint]bool
	groupSet  map[uint]bool
	forceMask []ObjectRef
}

// compile 把一条已通过 Normalize 的规则编译成可求值的形态。
func compile(r Rule) (*compiledRule, error) {
	n, err := Normalize(r)
	if err != nil {
		return nil, err
	}
	cr := &compiledRule{Rule: n, sceneSet: map[string]bool{}, instSet: map[uint]bool{}, groupSet: map[uint]bool{}}
	cr.ID, cr.ObserveSince, cr.UpdatedBy, cr.CreatedAt, cr.UpdatedAt = r.ID, r.ObserveSince, r.UpdatedBy, r.CreatedAt, r.UpdatedAt
	cr.Enabled, cr.Notify = r.Enabled, r.Notify
	cr.ref = RuleRef{ID: r.ID, Name: n.Name, Priority: n.Priority, Action: n.Action, Mode: n.Mode, Message: n.Message}
	for _, s := range n.Scenes {
		cr.sceneSet[s] = true
	}
	for _, id := range n.InstanceIDs {
		cr.instSet[id] = true
	}
	for _, id := range n.GroupIDs {
		cr.groupSet[id] = true
	}
	for _, c := range n.Conditions {
		cc := compiledCond{t: c.T}
		switch c.T {
		case CondStmt:
			list, _ := toStrings(c.V)
			cc.stmts = map[string]bool{}
			for _, s := range list {
				cc.stmts[s] = true
			}
			cc.label = fmt.Sprintf("语句类型 %s", strings.Join(list, " / "))
		case CondFeature:
			cc.feature = c.V.(string)
			cc.label = featureHit[cc.feature]
		case CondKeyword:
			cc.phrases, _ = toStrings(c.V)
		case CondRegex:
			cc.reSrc = c.V.(string)
			cc.re = regexp.MustCompile("(?i)" + cc.reSrc) // Normalize 已校验
		case CondObject:
			cc.cost = costObjects
			list, _ := toStrings(c.V)
			for _, p := range list {
				db, tb, col := splitPattern(p)
				cc.patterns = append(cc.patterns, objPattern{db, tb, col})
			}
		case CondScanRows, CondAffectRows:
			cc.n, _ = toInt64(c.V)
			cc.cost = costExplain
		case CondTime:
			cc.timeKind = c.V.(string)
		case CondFingerprint:
			cc.fp = c.V.(string)
		}
		cr.conds = append(cr.conds, cc)
	}
	// 条件按成本排序（稳定）：便宜的先求值，“全部满足”里前面不满足就不再取后面的事实
	sort.SliceStable(cr.conds, func(i, j int) bool { return cr.conds[i].cost < cr.conds[j].cost })
	cr.forceMask = forceRefs(n.Conditions)
	return cr, nil
}

// inWorkHours 判断是否在工作时间：周一到周五 09:00–19:00（服务时区）。
func inWorkHours(t time.Time) bool {
	switch t.Weekday() {
	case time.Saturday, time.Sunday:
		return false
	}
	return t.Hour() >= 9 && t.Hour() < 19
}

// eval 对一个条件求值，返回三值结果与说明（命中时是“命中了什么”，未知时是原因）。
func (c *compiledCond) eval(f *facts) (tri, string) {
	switch c.t {
	case CondStmt:
		if c.stmts[f.kind] {
			return yes, fmt.Sprintf("语句类型是 %s", f.kind)
		}
	case CondFeature:
		hit := false
		switch c.feature {
		case FeatNoWhere:
			hit = !f.hasWhere && f.hasFrom && (f.kind == "UPDATE" || f.kind == "DELETE" || f.kind == "SELECT")
			if f.kind == "UPDATE" {
				hit = !f.hasWhere
			}
		case FeatNoLimit:
			hit = f.kind == "SELECT" && !f.hasLimit
		case FeatCartesian:
			hit = f.cartesian
		case FeatMultiStmt:
			hit = f.multi
		case FeatSelectStar:
			hit = f.star
		case FeatLeadingWildcard:
			hit = f.leadingWild
		case FeatBigIn:
			hit = f.bigIn
		}
		if hit {
			return yes, c.label
		}
	case CondKeyword:
		for _, p := range c.phrases {
			if f.hasPhrase(p) {
				return yes, fmt.Sprintf("包含 %s", strings.ToUpper(p))
			}
		}
	case CondRegex:
		if c.re.MatchString(f.clean) {
			return yes, fmt.Sprintf("匹配正则 /%s/", c.reSrc)
		}
	case CondObject:
		objs, err := f.getObjects()
		if err != nil {
			return unknown, fmt.Sprintf("未评估：无法解析语句涉及的库表（%s）", err.Error())
		}
		for _, p := range c.patterns {
			for _, o := range objs {
				if !strings.EqualFold(o.Table, p.table) && p.table != "*" {
					continue
				}
				if p.db != "*" && !strings.EqualFold(o.Schema, p.db) {
					continue
				}
				if p.col != "" && p.col != "*" && !f.idents[strings.ToLower(p.col)] {
					continue
				}
				return yes, fmt.Sprintf("涉及 %s", patternText(p))
			}
			if p.table == "*" && p.db == "*" && p.col != "" && p.col != "*" && f.idents[strings.ToLower(p.col)] {
				return yes, fmt.Sprintf("涉及列 %s", p.col) // 任意库表的同名列：语句里出现该标识符即算（宁多勿漏）
			}
		}
	case CondScanRows:
		n, err := f.getScanRows()
		if err != nil {
			return unknown, fmt.Sprintf("未评估：无法估算扫描行数（%s）", err.Error())
		}
		if n >= c.n {
			return yes, fmt.Sprintf("预估扫描 %d 行 ≥ %d", n, c.n)
		}
	case CondAffectRows:
		n, basis, err := f.getAffectRows()
		if err != nil {
			return unknown, fmt.Sprintf("未评估：%s", err.Error())
		}
		if n >= c.n {
			if basis == "" { // 没有依据说明时用不带 %s 的整句，便于按目录翻译
				return yes, fmt.Sprintf("预估影响 %d 行 ≥ %d", n, c.n)
			}
			return yes, fmt.Sprintf("预估影响 %d 行 ≥ %d%s", n, c.n, basis)
		}
	case CondTime:
		off := !inWorkHours(f.now)
		if (c.timeKind == "offhours") == off {
			if off {
				return yes, fmt.Sprintf("当前是非工作时间（%s）", f.now.Format("15:04"))
			}
			return yes, fmt.Sprintf("当前是工作时间（%s）", f.now.Format("15:04"))
		}
	case CondFingerprint:
		if f.fpID == c.fp {
			return yes, fmt.Sprintf("语句指纹 %s 在黑名单里", c.fp)
		}
	}
	return no, ""
}

func patternText(p objPattern) string {
	s := p.db + "." + p.table
	if p.col != "" {
		s += "." + p.col
	}
	return s
}

// inScope 判断规则的适用范围是否覆盖本次请求；不覆盖时返回原因。
func (r *compiledRule) inScope(req *Request, groups map[uint]bool) (bool, string) {
	if !r.sceneSet[req.Scene] {
		return false, "场景不适用"
	}
	if r.Env != "all" && r.Env != req.Env {
		return false, "实例环境不适用"
	}
	if len(r.instSet) > 0 && !r.instSet[req.InstanceID] {
		return false, "实例不在范围内"
	}
	switch r.GroupMode {
	case "only":
		for id := range r.groupSet {
			if groups[id] {
				return true, ""
			}
		}
		return false, "用户不在适用的权限组里"
	case "except":
		for id := range r.groupSet {
			if groups[id] {
				return false, "用户所在权限组被排除"
			}
		}
	}
	return true, ""
}

// exceptionFor 返回命中的未过期例外（没有则 nil）。
func exceptionFor(exs []Exception, ruleID uint, req *Request, groups map[uint]bool, fp string, now time.Time) *Exception {
	for i := range exs {
		e := &exs[i]
		if e.RuleID != ruleID || !e.ExpiresAt.After(now) {
			continue
		}
		switch e.SubjectKind {
		case SubjectUser:
			if strings.EqualFold(e.SubjectValue, req.User) {
				return e
			}
		case SubjectGroup:
			if id, err := strconv.ParseUint(e.SubjectValue, 10, 32); err == nil && groups[uint(id)] {
				return e
			}
		case SubjectInstance:
			if e.SubjectValue == strconv.FormatUint(uint64(req.InstanceID), 10) {
				return e
			}
		case SubjectFingerprint:
			if strings.EqualFold(e.SubjectValue, fp) {
				return e
			}
		}
	}
	return nil
}

// evaluate 对一组已编译规则求值并合并成决定。globalObserve 为真时所有规则都按观察态处理。
func evaluate(rules []*compiledRule, exs []Exception, req *Request, groups map[uint]bool, globalObserve bool, now time.Time, obs ruleObserver) *Decision {
	d := &Decision{}
	f := newFacts(req, now)
	var live []evalHit
	for _, r := range rules {
		if ok, why := r.inScope(req, groups); !ok {
			d.Skipped = append(d.Skipped, Skip{Rule: r.ref, Reason: why})
			continue
		}
		if e := exceptionFor(exs, r.ID, req, groups, f.fpID, now); e != nil {
			d.Skipped = append(d.Skipped, Skip{Rule: r.ref, Reason: fmt.Sprintf("例外：%s（至 %s）", e.Reason, e.ExpiresAt.Format("2006-01-02"))})
			continue
		}
		t0 := time.Now()
		hit, matched, unk := r.match(f)
		if obs != nil {
			obs.observe(r.ID, time.Since(t0), len(unk) > 0)
		}
		if !hit {
			if len(unk) > 0 { // 没命中，但有条件没评估成：记在跳过里，测试台与命中详情能解释“为什么这条没拦”
				d.Skipped = append(d.Skipped, Skip{Rule: r.ref, Reason: strings.Join(unk, "；"), Unknown: true})
			}
			continue
		}
		h := Hit{Rule: r.ref, Matched: matched, Unknown: unk}
		if r.Mode == ModeObserve || globalObserve {
			h.Effective, h.WouldAction = "observe", r.Action
		} else {
			h.Effective = r.Action
			if why := downgrade(r.Action, req.Caps); why != "" {
				h.Effective, h.Downgraded = ActWarn, why
			}
		}
		d.Hits = append(d.Hits, h)
		if h.Effective != "observe" {
			live = append(live, evalHit{r: r, h: h})
		}
	}
	d.Facts = f.summary()
	merge(d, live, req)
	return d
}

type evalHit struct {
	r *compiledRule
	h Hit
}

// downgrade 在调用方没有对应能力时返回降级原因（空串 = 不降级）。
func downgrade(action string, c Caps) string {
	switch {
	case action == ActReason && !c.Reason:
		return "当前场景无法向用户收集原因，已降级为仅告警"
	case action == ActLimit && !c.Limit:
		return "当前场景无法收紧返回行数，已降级为仅告警"
	case action == ActMask && !c.Mask:
		return "当前场景无法强制脱敏，已降级为仅告警"
	}
	return ""
}

// match 对规则的条件求值：all 要求全部命中（有未知即不命中），any 要求至少一个命中。
// 返回是否命中、命中的条件说明、未能评估的说明。
func (r *compiledRule) match(f *facts) (bool, []string, []string) {
	var matched, unk []string
	hits, unknowns := 0, 0
	for i := range r.conds {
		c := &r.conds[i]
		if c.cost == costExplain {
			// EXPLAIN 是最贵的事实：规则已经确定（任一满足里前面已命中 / 全部满足里前面已未知）时不再去取
			if (r.Match == "any" && hits > 0) || (r.Match == "all" && unknowns > 0) {
				continue
			}
		}
		st, why := c.eval(f)
		switch st {
		case yes:
			hits++
			matched = append(matched, why)
		case unknown:
			unknowns++
			unk = append(unk, why)
		default:
			if r.Match == "all" {
				return false, nil, unk // 短路：全部满足里已有一个不命中
			}
		}
	}
	if r.Match == "all" {
		if hits == len(r.conds) {
			return true, matched, nil
		}
		return false, nil, unk
	}
	return hits > 0, matched, unk
}

// merge 把生效的命中合并成最终决定：任一 deny 即拒绝；reason 任一即需要；limit 取最小；mask 取并集；warn 放行并提示。
func merge(d *Decision, live []evalHit, req *Request) {
	var deny *evalHit
	seenMsg := map[string]bool{}
	seenMask := map[ObjectRef]bool{}
	for i := range live {
		eh := &live[i]
		r := eh.r
		switch eh.h.Effective {
		case ActDeny:
			if deny == nil || r.Priority < deny.r.Priority || (r.Priority == deny.r.Priority && r.ID < deny.r.ID) {
				deny = eh
			}
		case ActReason:
			d.NeedReason = true
			if m := ruleMessage(r, fmt.Sprintf("该语句命中规则「%s」，需要说明原因。", r.Name)); !seenMsg[m] {
				seenMsg[m] = true
				d.ReasonMsg = append(d.ReasonMsg, m)
			}
		case ActLimit:
			if d.MaxRows == 0 || r.ActionArg < d.MaxRows {
				d.MaxRows, d.LimitedBy = r.ActionArg, r.Name
			}
		case ActMask:
			for _, ref := range r.forceMask {
				if !seenMask[ref] {
					seenMask[ref] = true
					d.ForceMask = append(d.ForceMask, ref)
				}
			}
		case ActWarn:
			if m := ruleMessage(r, fmt.Sprintf("该语句命中规则「%s」。", r.Name)); !seenMsg[m] {
				seenMsg[m] = true
				d.Warnings = append(d.Warnings, m)
			}
		}
	}
	if deny != nil {
		ref := deny.r.ref
		d.Deny, d.Rule = true, &ref
		d.Message = ruleMessage(deny.r, fmt.Sprintf("该语句被 SQL 防火墙规则「%s」拒绝。", deny.r.Name))
	}
}

func ruleMessage(r *compiledRule, def string) string {
	if r.Message != "" {
		return r.Message
	}
	return def
}
