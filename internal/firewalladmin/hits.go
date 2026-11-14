// @author: kevinkelly
// @license: Apache Licence
// @file: hits.go
// @time: 2026/10/28

package firewalladmin

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/firewall"
	"dbtool/internal/model"
)

var hitResults = map[string]bool{
	"": true, firewall.ActDeny: true, firewall.ActReason: true, firewall.ActLimit: true,
	firewall.ActMask: true, firewall.ActWarn: true, "observe": true,
}

// hitView 把一行命中记录转成展示形态；rules 用来标注规则是否还在、当前的提示文案。
func hitView(h model.FirewallHit, rules map[uint]model.FirewallRule) HitView {
	v := HitView{
		ID: h.ID, CreatedAt: timeText(h.CreatedAt), LastAt: timeText(h.LastAt), RuleID: h.RuleID, RuleName: h.RuleName,
		Scene: h.Scene, User: h.UserName, InstanceID: h.InstanceID, Instance: h.InstanceName, DB: h.DBName,
		Effective: h.Effective, WouldAction: h.WouldAction, Fingerprint: h.Fingerprint, SQL: h.SQLText, Reason: h.ReasonText,
		Repeat: h.RepeatCnt, QueryLogID: h.QueryLogID, Matched: []string{}, Unknown: []string{},
	}
	if r, ok := rules[h.RuleID]; ok {
		v.RuleExists, v.Message = true, r.Message
		v.RuleName = r.Name
	}
	var m struct {
		Matched    []string `json:"matched"`
		Unknown    []string `json:"unknown"`
		Downgraded string   `json:"downgraded"`
	}
	if h.Matched != "" && json.Unmarshal([]byte(h.Matched), &m) == nil {
		v.Matched, v.Unknown, v.Downgraded = nz(m.Matched), nz(m.Unknown), m.Downgraded
	}
	return v
}

func (s *Service) ruleMap() (map[uint]model.FirewallRule, error) {
	rows, err := s.st.ListRules()
	if err != nil {
		return nil, err
	}
	out := make(map[uint]model.FirewallRule, len(rows))
	for _, m := range rows {
		out[m.ID] = m
	}
	return out, nil
}

// Hits 返回命中记录的一页（新的在前）。
func (s *Service) Hits(q HitQuery) (*HitList, error) {
	if !hitResults[q.Result] {
		return nil, badReq(fmt.Sprintf("不认识的结果类型 %s", q.Result))
	}
	if q.Scene != "" && !contains(firewall.Scenes, q.Scene) {
		return nil, badReq(fmt.Sprintf("不认识的场景 %s", q.Scene))
	}
	if q.Days <= 0 {
		q.Days = 7
	}
	q.Days = min(q.Days, 90)
	if q.Page <= 0 {
		q.Page = 1
	}
	if q.Size <= 0 {
		q.Size = 20
	}
	q.Size = min(q.Size, 100)
	q.Q = truncate(strings.TrimSpace(q.Q), 100)
	rows, total, err := s.st.ListHits(HitFilter{
		RuleID: q.RuleID, Q: q.Q, User: strings.TrimSpace(q.User), Result: q.Result, Scene: q.Scene,
		Since: dayStart(s.now()).AddDate(0, 0, -(q.Days - 1)), Offset: (q.Page - 1) * q.Size, Limit: q.Size,
	})
	if err != nil {
		return nil, err
	}
	rules, err := s.ruleMap()
	if err != nil {
		return nil, err
	}
	out := &HitList{Items: make([]HitView, 0, len(rows)), Total: total, Page: q.Page, Size: q.Size}
	for _, h := range rows {
		out.Items = append(out.Items, hitView(h, rules))
	}
	return out, nil
}

// Hit 返回一条命中记录的详情。
func (s *Service) Hit(id uint) (*HitView, error) {
	h, err := s.st.GetHit(id)
	if err != nil {
		return nil, err
	}
	if h == nil {
		return nil, notFound("命中记录不存在")
	}
	rules, err := s.ruleMap()
	if err != nil {
		return nil, err
	}
	v := hitView(*h, rules)
	return &v, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---- 例外 ----

const (
	maxExceptionDays = 365
	exceptionKeep    = 30 * 24 * time.Hour // 过期后在名单里继续显示多久（解释“为什么不再生效”）
)

var fingerprintRe = regexp.MustCompile(`^[0-9a-f]{8}$`)

// Exceptions 返回例外名单：有效的，以及 30 天内刚过期的（标记 expired）。
func (s *Service) Exceptions() ([]ExceptionView, error) {
	now := s.now()
	rows, err := s.st.ListExceptions(now.Add(-exceptionKeep))
	if err != nil {
		return nil, err
	}
	rules, err := s.ruleMap()
	if err != nil {
		return nil, err
	}
	gname, iname, err := s.subjectNames()
	if err != nil {
		return nil, err
	}
	out := make([]ExceptionView, 0, len(rows))
	for _, e := range rows {
		v := ExceptionView{ID: e.ID, RuleID: e.RuleID, SubjectKind: e.SubjectKind, SubjectValue: e.SubjectValue, Reason: e.Reason,
			ExpiresAt: e.ExpiresAt.Format("2006-01-02"), Expired: !e.ExpiresAt.After(now), CreatedBy: e.CreatedBy, CreatedAt: timeText(e.CreatedAt)}
		if r, ok := rules[e.RuleID]; ok {
			v.RuleName = r.Name
		} else {
			v.RuleName = "（规则已删除）"
		}
		v.SubjectText = subjectText(e, gname, iname)
		out = append(out, v)
	}
	return out, nil
}

// subjectNames 返回权限组与实例的 ID → 名称，用来把例外的主体显示成人能读的文字。
func (s *Service) subjectNames() (groups, insts map[uint]string, err error) {
	gs, err := s.st.Groups()
	if err != nil {
		return nil, nil, err
	}
	groups = map[uint]string{}
	for _, g := range gs {
		groups[g.ID] = g.Name
	}
	is, err := s.instances()
	if err != nil {
		return nil, nil, err
	}
	insts = map[uint]string{}
	for _, i := range is {
		insts[i.ID] = i.Name
	}
	return groups, insts, nil
}

func subjectText(e model.FirewallException, groups, insts map[uint]string) string {
	id, _ := strconv.ParseUint(e.SubjectValue, 10, 32)
	switch e.SubjectKind {
	case firewall.SubjectUser:
		return fmt.Sprintf("用户 %s", e.SubjectValue)
	case firewall.SubjectGroup:
		if n, ok := groups[uint(id)]; ok {
			return fmt.Sprintf("权限组 %s", n)
		}
		return fmt.Sprintf("权限组 #%s", e.SubjectValue)
	case firewall.SubjectInstance:
		if n, ok := insts[uint(id)]; ok {
			return fmt.Sprintf("实例 %s", n)
		}
		return fmt.Sprintf("实例 #%s", e.SubjectValue)
	case firewall.SubjectFingerprint:
		return fmt.Sprintf("SQL 指纹 %s", e.SubjectValue)
	}
	return e.SubjectKind + " " + e.SubjectValue
}

// CreateException 添加例外。必须写原因（4–500 字符）并有有效期（1–365 天）：到期自动失效，不依赖清理任务。
func (s *Service) CreateException(actor audit.Actor, in ExceptionInput) (*ExceptionView, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(in.Reason)
	if n := len([]rune(reason)); n < 4 || n > firewall.MaxTextLen {
		return nil, badReq(fmt.Sprintf("请填写原因（4–%d 个字符）：说明这条规则为什么对该对象不适用", firewall.MaxTextLen))
	}
	if in.Days < 1 || in.Days > maxExceptionDays {
		return nil, badReq(fmt.Sprintf("有效期需在 1–%d 天之间", maxExceptionDays))
	}
	rm, err := s.st.GetRule(in.RuleID)
	if err != nil {
		return nil, err
	}
	if rm == nil {
		return nil, badReq("请选择要豁免的规则")
	}
	value := strings.TrimSpace(in.SubjectValue)
	switch in.SubjectKind {
	case firewall.SubjectUser:
		if value == "" || len([]rune(value)) > 100 {
			return nil, badReq("请填写用户名")
		}
	case firewall.SubjectGroup:
		id, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return nil, badReq("请选择权限组")
		}
		groups, err := s.st.Groups()
		if err != nil {
			return nil, err
		}
		found := false
		for _, g := range groups {
			found = found || g.ID == uint(id)
		}
		if !found {
			return nil, badReq("权限组不存在")
		}
		value = strconv.FormatUint(id, 10)
	case firewall.SubjectInstance:
		id, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return nil, badReq("请选择实例")
		}
		insts, err := s.instances()
		if err != nil {
			return nil, err
		}
		found := false
		for _, i := range insts {
			found = found || i.ID == uint(id)
		}
		if !found {
			return nil, badReq("实例不存在")
		}
		value = strconv.FormatUint(id, 10)
	case firewall.SubjectFingerprint:
		value = strings.ToLower(value)
		if !fingerprintRe.MatchString(value) {
			return nil, badReq("SQL 指纹是 8 位十六进制（可从命中记录或测试台里复制）")
		}
	default:
		return nil, badReq(fmt.Sprintf("不认识的豁免对象类型 %s", in.SubjectKind))
	}
	now := s.now()
	m := &model.FirewallException{RuleID: in.RuleID, SubjectKind: in.SubjectKind, SubjectValue: value, Reason: reason,
		ExpiresAt: now.AddDate(0, 0, in.Days), CreatedBy: actor.Username, CreatedAt: now}
	gname, iname, err := s.subjectNames()
	if err != nil {
		return nil, err
	}
	subject := subjectText(*m, gname, iname)
	entry := s.auditEntry(actor, actException, fmt.Sprintf("为规则「%s」添加例外：%s，有效期至 %s，原因：%s", // i18n-ignore 审计文本，存库后不翻译
		rm.Name, subject, m.ExpiresAt.Format("2006-01-02"), reason), false)
	if err := s.st.CreateException(m, entry); err != nil {
		return nil, err
	}
	s.invalidate()
	list, err := s.Exceptions()
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ID == m.ID {
			return &list[i], nil
		}
	}
	return &ExceptionView{ID: m.ID, RuleID: m.RuleID, RuleName: rm.Name, SubjectKind: m.SubjectKind, SubjectValue: value,
		SubjectText: subject, Reason: reason, ExpiresAt: m.ExpiresAt.Format("2006-01-02"), CreatedBy: actor.Username}, nil
}

// DeleteException 撤销一条例外。
func (s *Service) DeleteException(actor audit.Actor, id uint) error {
	e, err := s.st.GetException(id)
	if err != nil {
		return err
	}
	if e == nil {
		return notFound("例外不存在")
	}
	rules, err := s.ruleMap()
	if err != nil {
		return err
	}
	name := "（规则已删除）" // i18n-ignore 审计文本，存库后不翻译
	if r, ok := rules[e.RuleID]; ok {
		name = r.Name
	}
	gname, iname, err := s.subjectNames()
	if err != nil {
		return err
	}
	entry := s.auditEntry(actor, actException, fmt.Sprintf("撤销规则「%s」的例外：%s（原因：%s；原到期 %s）", // i18n-ignore 审计文本，存库后不翻译
		name, subjectText(*e, gname, iname), e.Reason, e.ExpiresAt.Format("2006-01-02")), false)
	if err := s.st.DeleteException(id, entry); err != nil {
		return err
	}
	s.invalidate()
	return nil
}
