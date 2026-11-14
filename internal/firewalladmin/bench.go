// @author: kevinkelly
// @license: Apache Licence
// @file: bench.go
// @time: 2026/10/28

package firewalladmin

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"dbtool/internal/firewall"
	"dbtool/internal/instext"
)

// 语句测试台与历史回放。两者都用 firewall.Simulator 离线求值：不读规则快照、不写命中记录、不发通知。
// 库表解析只用词法（线上有 goInception 时更准确），页面上如实注明。

const (
	maxBenchSQL        = 200 << 10
	replayMaxRows      = 20000            // 回放最多评估这么多条语句（设计 §10.1）
	replayMaxWorkflows = 2000             // 工单数据源最多取最近这么多张工单
	replayBudget       = 10 * time.Second // 回放的时间预算
	replayMaxDays      = 30
	replaySamples      = 5
	replayAdviceAt     = 0.01 // 命中占比超过它，建议先观察
)

// whenTime 把测试台的“时间”选项换算成一个具体时刻（time 条件按它判断；工作时间 = 周一到周五 9–19 点）。
func whenTime(when string, now time.Time) time.Time {
	at := func(d time.Time, h int) time.Time {
		return time.Date(d.Year(), d.Month(), d.Day(), h, 0, 0, 0, now.Location())
	}
	weekday := now
	for weekday.Weekday() == time.Saturday || weekday.Weekday() == time.Sunday {
		weekday = weekday.AddDate(0, 0, -1)
	}
	switch when {
	case "workday_day":
		return at(weekday, 14)
	case "workday_night":
		return at(weekday, 3)
	case "workday_late":
		return at(weekday, 22)
	case "weekend":
		d := now
		for d.Weekday() != time.Saturday {
			d = d.AddDate(0, 0, -1)
		}
		return at(d, 11)
	}
	return now
}

// benchTables 是词法级的库表解析（见 firewall.LexicalObjects）。
func benchTables(stmt string) []firewall.Object { return firewall.LexicalObjects(stmt) }

func capsOf(scene string) firewall.Caps {
	switch scene {
	case firewall.SceneQuery:
		return firewall.FullCaps
	case firewall.SceneExport:
		return firewall.Caps{Limit: true, Mask: true}
	}
	return firewall.Caps{} // 工单：reason / limit / mask 不适用，按 warn 处理（设计 §9.2）
}

// wordRe 按“词”匹配 p（不区分大小写）：两端是字母数字下划线时要求有词边界，避免 users 高亮到 sys_users 里面。
func wordRe(p string) *regexp.Regexp {
	isWord := func(r byte) bool {
		return r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= 0x80
	}
	pre, post := "", ""
	if len(p) > 0 && isWord(p[0]) {
		pre = `\b`
	}
	if len(p) > 0 && isWord(p[len(p)-1]) {
		post = `\b`
	}
	return regexp.MustCompile("(?i)" + pre + regexp.QuoteMeta(p) + post)
}

// highlights 返回规则的条件在语句里命中的文字片段（页面据此高亮）。只是展示用，不影响决定。
func highlights(conds []firewall.Condition, sql string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" && !seen[strings.ToLower(s)] {
			seen[strings.ToLower(s)] = true
			out = append(out, s)
		}
	}
	find := func(p string) {
		if p == "" || p == "*" {
			return
		}
		if m := wordRe(p).FindString(sql); m != "" {
			add(m)
		}
	}
	for _, c := range conds {
		switch c.T {
		case firewall.CondKeyword:
			for _, k := range strList(c.V) {
				find(k)
			}
		case firewall.CondRegex:
			if re, err := regexp.Compile("(?i)" + fmt.Sprint(c.V)); err == nil {
				if m := re.FindString(sql); m != "" && len(m) <= 80 {
					add(m)
				}
			}
		case firewall.CondObject:
			for _, p := range strList(c.V) {
				parts := strings.Split(p, ".")
				for _, part := range parts[max(0, len(parts)-2):] { // 表与列；库名一般不是命中的原因
					find(part)
				}
			}
		}
	}
	return out
}

// loadSimRules 读出全部规则，分成“参与求值”和“已停用 / 无效”两类，供测试台与回放使用。
func (s *Service) loadSimRules() (active []firewall.Rule, byID map[uint]firewall.Rule, skipped []firewall.Skip, notes []string, err error) {
	rows, err := s.st.ListRules()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	byID = map[uint]firewall.Rule{}
	for _, m := range rows {
		r := firewall.RuleFromModel(m)
		byID[r.ID] = r
		ref := firewall.RuleRef{ID: r.ID, Name: r.Name, Priority: r.Priority, Action: r.Action, Mode: r.Mode, Message: r.Message}
		if !r.Enabled {
			skipped = append(skipped, firewall.Skip{Rule: ref, Reason: "规则已停用"})
			continue
		}
		if _, e := firewall.Normalize(r); e != nil {
			notes = append(notes, fmt.Sprintf("规则「%s」无效，没有参与测试（线上同样不会生效）：%s", r.Name, e.Error()))
			continue
		}
		active = append(active, r)
	}
	return active, byID, skipped, notes, nil
}

// Evaluate 对一条语句求值，返回完整决定与每条规则的命中解释。使用库里当前已保存的全部启用规则与未过期的例外。
func (s *Service) Evaluate(in EvaluateRequest) (*EvalResult, error) {
	sql := strings.TrimSpace(in.SQL)
	switch {
	case sql == "":
		return nil, badReq("请输入要测试的语句")
	case len(sql) > maxBenchSQL:
		return nil, badReq("语句过长")
	}
	scene := in.Scene
	if scene == "" {
		scene = firewall.SceneQuery
	}
	if !contains(firewall.Scenes, scene) {
		return nil, badReq(fmt.Sprintf("不认识的场景 %s", scene))
	}
	active, byID, skipped, notes, err := s.loadSimRules()
	if err != nil {
		return nil, err
	}
	now := s.now()
	exRows, err := s.st.ListExceptions(now)
	if err != nil {
		return nil, err
	}
	exs := make([]firewall.Exception, 0, len(exRows))
	for _, e := range exRows {
		exs = append(exs, firewall.Exception{ID: e.ID, RuleID: e.RuleID, SubjectKind: e.SubjectKind, SubjectValue: e.SubjectValue,
			Reason: e.Reason, ExpiresAt: e.ExpiresAt, CreatedBy: e.CreatedBy})
	}
	globalObserve := s.globalMode() == firewall.ModeObserve
	sim, err := firewall.NewSimulator(active, exs, globalObserve)
	if err != nil {
		return nil, err
	}
	req := firewall.Request{
		Scene: scene, User: strings.TrimSpace(in.User), GroupIDs: nz(in.GroupIDs), Env: in.Env, DB: strings.TrimSpace(in.DB),
		SQL: sql, Caps: capsOf(scene), AffectRows: -1, Now: whenTime(in.When, now),
		Tables: func() ([]firewall.Object, error) { return benchTables(sql), nil },
	}
	if req.Env != "" && !instext.ValidEnv(req.Env) {
		return nil, badReq("实例环境不在环境字典里")
	}
	if in.InstanceID != 0 {
		insts, err := s.instances()
		if err != nil {
			return nil, err
		}
		found := false
		for _, i := range insts {
			if i.ID == in.InstanceID {
				req.InstanceID, req.InstanceName, req.DBType, req.Env, found = i.ID, i.Name, i.DBType, i.Env, true
			}
		}
		if !found {
			return nil, badReq("实例不存在")
		}
	}
	if in.ScanRows != nil {
		v := *in.ScanRows
		req.ScanRows = func() (int64, error) { return v, nil }
	}
	if in.AffectRows != nil {
		req.AffectRows = *in.AffectRows
	}
	d := sim.Run(req)

	res := &EvalResult{
		Message: d.Message, NeedReason: d.NeedReason, ReasonMsgs: nz(d.ReasonMsg), MaxRows: d.MaxRows, LimitedBy: d.LimitedBy,
		Warnings: nz(d.Warnings), Facts: d.Facts, GlobalMode: s.globalMode(), EnabledFlag: s.switchOn(), Caps: req.Caps,
		Failed: d.Failed, Deny: d.Deny, Rule: d.Rule, ForceMask: []string{}, Hits: []EvalHit{}, Highlight: []string{}, Notes: notes,
	}
	if len(res.Facts.Objects) == 0 { // 求值只在有 object 条件时才取库表；测试台总是展示语句涉及的对象
		res.Facts.Objects = []string{}
		for _, o := range benchTables(sql) {
			res.Facts.Objects = append(res.Facts.Objects, strings.TrimPrefix(o.Schema+"."+o.Table, "."))
		}
	}
	for _, o := range d.ForceMask {
		res.ForceMask = append(res.ForceMask, strings.Trim(o.Schema+"."+o.Table+"."+o.Column, "."))
	}
	res.Skipped = append(d.Skipped, skipped...)
	if res.Skipped == nil {
		res.Skipped = []firewall.Skip{}
	}
	hl := map[string]bool{}
	for _, h := range d.Hits {
		eh := EvalHit{Rule: h.Rule, Effective: h.Effective, WouldAction: h.WouldAction, Matched: nz(h.Matched), Unknown: nz(h.Unknown),
			Downgraded: h.Downgraded, Highlight: []string{}}
		if r, ok := byID[h.Rule.ID]; ok {
			eh.Highlight = nz(highlights(r.Conditions, sql))
		}
		for _, w := range eh.Highlight {
			if !hl[strings.ToLower(w)] {
				hl[strings.ToLower(w)] = true
				res.Highlight = append(res.Highlight, w)
			}
		}
		res.Hits = append(res.Hits, eh)
	}
	switch {
	case d.Deny:
		res.Verdict = firewall.ActDeny
	case d.NeedReason:
		res.Verdict = firewall.ActReason
	case d.MaxRows > 0:
		res.Verdict = firewall.ActLimit
	case len(d.ForceMask) > 0:
		res.Verdict = firewall.ActMask
	case len(d.Warnings) > 0:
		res.Verdict = firewall.ActWarn
	default:
		res.Verdict = "pass"
	}
	if !activeScenes[scene] {
		res.Notes = append(res.Notes, fmt.Sprintf("“%s”场景当前版本还没有接入执行：这里的结果是规则配好之后会得到的决定，线上暂时不会拦截。", sceneLabels[scene]))
	}
	if in.ScanRows != nil || in.AffectRows != nil {
		res.Notes = append(res.Notes, "预估扫描 / 影响行数是手填的，用来试条件。线上：预估扫描行数当前版本取不到（EXPLAIN 尚未接入）；预估影响行数只在 SQL 工单场景有值（来自 goInception 检查结果）。")
	}
	if scene == firewall.SceneWorkflow {
		res.Notes = append(res.Notes, "SQL 工单场景按“一条语句一次求值”：提交检查时对检查结果的每一行求值，执行前再复核一次（只认拒绝）；要原因 / 限行 / 脱敏在工单里没有对应能力，会降级为仅告警。")
	}
	res.Notes = append(res.Notes, "库表来自词法解析；线上配置了 goInception 时用语法树，别名、子查询的识别更准确。")
	return res, nil
}
