// @author: kevinkelly
// @license: Apache Licence
// @file: service.go
// @time: 2026/10/28

package redisrule

import (
	"fmt"
	"strings"
	"time"

	"dbtool/internal/audit"
)

// 规则管理（超级用户）的业务层：校验、二次确认、写审计（含变更前后差异）。HTTP 层见 internal/redisrule/admin。

// APIError 是带 HTTP 状态的业务错误。
type APIError struct {
	Status int
	Code   string
	Msg    string
}

func (e *APIError) Error() string { return e.Msg }

func apiErr(status int, code, msg string) error { return &APIError{status, code, msg} }

// Service 是规则管理服务。
type Service struct {
	repo  Repo
	audit any // audit.LifecycleWriter
	now   func() time.Time
}

// NewService 创建服务。auditSink 需实现 audit.LifecycleWriter（为 nil 则不写审计，仅测试）。
func NewService(repo Repo, auditSink any) *Service {
	return &Service{repo: repo, audit: auditSink, now: time.Now}
}

// SetClock 仅测试使用。
func (s *Service) SetClock(f func() time.Time) { s.now = f }

// Repo 暴露存储（页面需要判断表是否存在）。
func (s *Service) Repo() Repo { return s.repo }

// Ready 规则表是否已建。
func (s *Service) Ready() bool { return s.repo.Ready() }

// List 返回全部规则。
func (s *Service) List() ([]Rule, error) { return s.repo.List() }

func (s *Service) needReady() error {
	if !s.repo.Ready() {
		return apiErr(503, "table_missing", ErrTableMissing.Error())
	}
	return nil
}

func needConfirm(confirm bool) error {
	if !confirm {
		return apiErr(400, "confirm_required", "规则变更需要二次确认（confirm=true）")
	}
	return nil
}

func (s *Service) auditLog(p audit.Actor, action, extra string) {
	if s.audit != nil {
		audit.Lifecycle(s.audit, p, action, extra, s.now())
	}
}

func storeErr(err error) error {
	switch err {
	case nil:
		return nil
	case ErrNameTaken:
		return apiErr(409, "name_taken", ErrNameTaken.Error())
	case ErrNotFound:
		return apiErr(404, "not_found", ErrNotFound.Error())
	case ErrTableMissing:
		return apiErr(503, "table_missing", ErrTableMissing.Error())
	}
	return err
}

func validate(in Rule) (Rule, error) {
	r, err := Normalize(in)
	if err != nil {
		return r, apiErr(400, "bad_rule", err.Error())
	}
	return r, nil
}

// Create 新增规则（confirm 必须为 true）。
func (s *Service) Create(p audit.Actor, in Rule, confirm bool) (*Rule, error) {
	if err := needConfirm(confirm); err != nil {
		return nil, err
	}
	if err := s.needReady(); err != nil {
		return nil, err
	}
	r, err := validate(in)
	if err != nil {
		return nil, err
	}
	all, err := s.repo.List()
	if err != nil {
		return nil, err
	}
	if len(all) >= MaxRules {
		return nil, apiErr(400, "too_many", fmt.Sprintf("规则最多 %d 条", MaxRules))
	}
	r.ID, r.CreatedBy, r.UpdatedBy, r.CreatedAt, r.UpdatedAt = 0, p.Username, p.Username, s.now(), s.now()
	if err := s.repo.Create(&r); err != nil {
		return nil, storeErr(err)
	}
	s.auditLog(p, "Redis上线规则-新增", fmt.Sprintf("规则 #%d「%s」：%s", r.ID, r.Name, describe(r))) // i18n-ignore 审计文本，存库后不翻译
	return &r, nil
}

// Update 修改规则（整体替换可编辑字段）。
func (s *Service) Update(p audit.Actor, id uint, in Rule, confirm bool) (*Rule, error) {
	if err := needConfirm(confirm); err != nil {
		return nil, err
	}
	if err := s.needReady(); err != nil {
		return nil, err
	}
	old, err := s.repo.Get(id)
	if err != nil {
		return nil, err
	}
	if old == nil {
		return nil, apiErr(404, "not_found", ErrNotFound.Error())
	}
	r, err := validate(in)
	if err != nil {
		return nil, err
	}
	r.ID, r.CreatedBy, r.CreatedAt, r.UpdatedBy, r.UpdatedAt = old.ID, old.CreatedBy, old.CreatedAt, p.Username, s.now()
	if err := s.repo.Update(&r); err != nil {
		return nil, storeErr(err)
	}
	s.auditLog(p, "Redis上线规则-修改", fmt.Sprintf("规则 #%d「%s」：%s", r.ID, old.Name, Diff(*old, r))) // i18n-ignore 审计文本，存库后不翻译
	return &r, nil
}

// SetEnabled 启用 / 停用。
func (s *Service) SetEnabled(p audit.Actor, id uint, enabled, confirm bool) (*Rule, error) {
	if err := needConfirm(confirm); err != nil {
		return nil, err
	}
	if err := s.needReady(); err != nil {
		return nil, err
	}
	old, err := s.repo.Get(id)
	if err != nil {
		return nil, err
	}
	if old == nil {
		return nil, apiErr(404, "not_found", ErrNotFound.Error())
	}
	r := *old
	r.Enabled, r.UpdatedBy, r.UpdatedAt = enabled, p.Username, s.now()
	if err := s.repo.Update(&r); err != nil {
		return nil, storeErr(err)
	}
	act := "停用" // i18n-ignore 审计文本，存库后不翻译
	if enabled {
		act = "启用" // i18n-ignore 审计文本，存库后不翻译
	}
	s.auditLog(p, "Redis上线规则-"+act, fmt.Sprintf("规则 #%d「%s」：启用 %s → %s", r.ID, r.Name, yn(old.Enabled), yn(enabled))) // i18n-ignore 审计文本，存库后不翻译
	return &r, nil
}

// Delete 删除规则。
func (s *Service) Delete(p audit.Actor, id uint, confirm bool) error {
	if err := needConfirm(confirm); err != nil {
		return err
	}
	if err := s.needReady(); err != nil {
		return err
	}
	old, err := s.repo.Get(id)
	if err != nil {
		return err
	}
	if old == nil {
		return apiErr(404, "not_found", ErrNotFound.Error())
	}
	if err := s.repo.Delete(id); err != nil {
		return storeErr(err)
	}
	s.auditLog(p, "Redis上线规则-删除", fmt.Sprintf("规则 #%d「%s」（删除前）：%s", old.ID, old.Name, describe(*old))) // i18n-ignore 审计文本，存库后不翻译
	return nil
}

// TestLines 用当前规则试检查一段命令（不执行、不写审计）。
func (s *Service) TestLines(instanceID uint, db int, text string) ([]LineVerdict, error) {
	lines, err := SplitLines(text)
	if err != nil {
		return nil, apiErr(400, "bad_request", err.Error())
	}
	rules, err := s.repo.List()
	if err != nil {
		return nil, err
	}
	out := make([]LineVerdict, len(lines))
	for i, l := range lines {
		out[i] = LineVerdict{Line: l.No, Text: l.Text, Verdict: EvaluateLine(rules, Target{InstanceID: instanceID, DB: db}, l.Text)}
	}
	return out, nil
}

// LineVerdict 是试检查的一行结果。
type LineVerdict struct {
	Line int    `json:"line"`
	Text string `json:"text"`
	Verdict
}

// ---- 变更差异 ----

func yn(b bool) string {
	if b {
		return "是" // i18n-ignore 审计文本，存库后不翻译
	}
	return "否" // i18n-ignore 审计文本，存库后不翻译
}

func short(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

type field struct{ label, val string }

func fields(r Rule) []field {
	ids := make([]string, len(r.InstanceIDs))
	for i, id := range r.InstanceIDs {
		ids[i] = fmt.Sprint(id)
	}
	dbs := make([]string, len(r.DBs))
	for i, d := range r.DBs {
		dbs[i] = fmt.Sprint(d)
	}
	orAll := func(s string) string {
		if s == "" {
			return "全部" // i18n-ignore 审计文本，存库后不翻译
		}
		return s
	}
	return []field{
		{"名称", r.Name}, {"启用", yn(r.Enabled)}, {"命令", strings.Join(r.Commands, ",")}, {"键模式", strings.Join(r.KeyPatterns, " | ")}, // i18n-ignore 审计文本，存库后不翻译
		{"实例", orAll(strings.Join(ids, ","))}, {"库", orAll(strings.Join(dbs, ","))}, {"键数上限", fmt.Sprint(r.MaxKeys)}, // i18n-ignore 审计文本，存库后不翻译
		{"记录执行前值", yn(r.RecordBefore)}, {"说明", r.Description}, {"备注", r.Remark}, // i18n-ignore 审计文本，存库后不翻译
	}
}

func describe(r Rule) string {
	var parts []string
	for _, f := range fields(r) {
		if f.val == "" {
			continue
		}
		parts = append(parts, f.label+"="+short(f.val, 80))
	}
	return strings.Join(parts, "；")
}

// Diff 把两版规则的差异写成“字段：旧 → 新”。
func Diff(a, b Rule) string {
	fa, fb := fields(a), fields(b)
	var parts []string
	for i := range fa {
		if fa[i].val != fb[i].val {
			parts = append(parts, fmt.Sprintf("%s：%s → %s", fa[i].label, short(fa[i].val, 80), short(fb[i].val, 80)))
		}
	}
	if len(parts) == 0 {
		return "没有字段变化" // i18n-ignore 审计文本，存库后不翻译
	}
	return strings.Join(parts, "；")
}
