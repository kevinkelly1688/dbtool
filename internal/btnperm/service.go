// @author: kevinkelly
// @license: Apache Licence
// @file: service.go
// @time: 2026/10/28

package btnperm

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"dbtool/internal/model"
)

// Error 是带 HTTP 状态的业务错误。
type Error struct {
	HTTP    int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string { return e.Message }

func newErr(status int, code, msg string) *Error {
	return &Error{HTTP: status, Code: code, Message: msg}
}
func bad(msg string) *Error      { return newErr(http.StatusBadRequest, "bad_request", msg) }
func notFound(msg string) *Error { return newErr(http.StatusNotFound, "not_found", msg) }
func conflict(msg string) *Error { return newErr(http.StatusConflict, "conflict", msg) }

// Actor 是操作者（写审计用）。
type Actor struct {
	ID       uint
	Username string
	Display  string
}

// Service 是按钮权限的管理服务：权限组按钮、单用户授权 / 禁用、无权限按钮的呈现方式。所有写操作在同一事务里先改数据再写 dbt_audit_log。
type Service struct {
	repo Repo
	res  *Resolver // 写操作后失效缓存；可为 nil
	now  func() time.Time
}

// NewService 创建服务。res 是进程内的解析器（写操作成功后使缓存失效）。
func NewService(repo Repo, res *Resolver) *Service {
	return &Service{repo: repo, res: res, now: time.Now}
}

// SetClock 替换时钟（测试用）。
func (s *Service) SetClock(f func() time.Time) { s.now = f }

func (s *Service) invalidate() {
	if s.res != nil {
		s.res.InvalidateAll()
	}
}

const maxAuditInfo = 4000

func (s *Service) audit(tx Repo, a Actor, action, info string) error {
	if r := []rune(info); len(r) > maxAuditInfo {
		info = string(r[:maxAuditInfo]) + "…"
	}
	return tx.InsertAudit(&model.AuditEntry{UserID: a.ID, UserName: cut(a.Username, 30), UserDisplay: cut(a.Display, 50), Action: action, ExtraInfo: info, ActionTime: s.now()})
}

func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// ---- 登记表输出 ----

// RegistryView 是权限树页面需要的全部静态数据。
type RegistryView struct {
	Modules     []Module            `json:"modules"`
	Presets     map[string][]string `json:"presets"`
	PresetNames []string            `json:"preset_names"`
	Total       int                 `json:"total"`
	Grantable   int                 `json:"grantable"`
	Danger      int                 `json:"danger"`
	Setting     Setting             `json:"setting"`
	TablesReady bool                `json:"tables_ready"`
}

// Registry 返回登记表、预设与当前设置（Setting.Mode 是迁移状态：还没迁移时页面顶部提示执行 dbt-admin perm migrate）。
func (s *Service) Registry() (*RegistryView, error) {
	set, err := s.repo.Setting()
	if err != nil {
		return nil, err
	}
	out := &RegistryView{Modules: Modules(), Presets: map[string][]string{}, PresetNames: PresetNames, Setting: set.Normalize(), TablesReady: true}
	for _, n := range PresetNames {
		out.Presets[n] = Preset(n)
	}
	out.Total, out.Grantable, out.Danger = Counts()
	if tr, ok := s.repo.(interface{ TablesReady() (bool, error) }); ok {
		if out.TablesReady, err = tr.TablesReady(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ---- 权限组 ----

// GroupView 是一个权限组的按钮授权。
type GroupView struct {
	Group GroupInfo `json:"group"`
	Codes []string  `json:"codes"`
}

// Group 返回权限组当前被授予的按钮（已登记且可授予的；历史遗留的未知码被忽略）。
func (s *Service) Group(id uint) (*GroupView, error) {
	g, err := s.repo.GetGroup(id)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, notFound("权限组不存在")
	}
	codes, err := s.repo.GroupButtons(id)
	if err != nil {
		return nil, err
	}
	return &GroupView{Group: *g, Codes: onlyValid(codes)}, nil
}

func onlyValid(in []string) []string {
	out := []string{}
	for _, c := range in {
		if Valid(c) {
			out = append(out, c)
		}
	}
	return sortedCopy(out)
}

// cleanCodes 校验按钮码：去重，必须已登记且可授予。
func cleanCodes(in []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, c := range in {
		c = strings.TrimSpace(c)
		if c == "" || seen[c] {
			continue
		}
		b, ok := Lookup(c)
		if !ok {
			return nil, bad(fmt.Sprintf("未知的按钮权限码：%s", c))
		}
		if !b.Grantable() {
			return nil, bad(fmt.Sprintf("%s（%s）仅限超级用户，不能授予", b.Label, c))
		}
		seen[c] = true
		out = append(out, c)
	}
	sort.Strings(out)
	return out, nil
}

// Diff 是两个集合的差异。
type Diff struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
}

func diffSets(cur, next []string) Diff {
	c, n := map[string]bool{}, map[string]bool{}
	for _, x := range cur {
		c[x] = true
	}
	for _, x := range next {
		n[x] = true
	}
	d := Diff{Added: []string{}, Removed: []string{}}
	for x := range n {
		if !c[x] {
			d.Added = append(d.Added, x)
		}
	}
	for x := range c {
		if !n[x] {
			d.Removed = append(d.Removed, x)
		}
	}
	sort.Strings(d.Added)
	sort.Strings(d.Removed)
	return d
}

// noViewWarnings 返回“勾选了操作按钮但页面上没有任何查看类按钮”的提示：用户将无法打开该页面。
func noViewWarnings(codes []string) []string {
	on := map[string]bool{}
	for _, c := range codes {
		on[c] = true
	}
	var out []string
	for _, m := range Tree {
		for _, p := range m.Pages {
			hasOp, hasView := false, false
			for _, b := range p.Buttons {
				if !on[b.Code] {
					continue
				}
				if b.Kind == KindView {
					hasView = true
				} else {
					hasOp = true
				}
			}
			if hasOp && !hasView {
				out = append(out, fmt.Sprintf("页面「%s」勾选了操作按钮但没有任何查看类按钮，用户将打不开该页面", p.Name))
			}
		}
	}
	return out
}

// GroupDiff 计算把权限组改成 codes 的差异（不写库），供保存前确认。
func (s *Service) GroupDiff(id uint, codes []string) (*SaveResult, error) {
	g, err := s.Group(id)
	if err != nil {
		return nil, err
	}
	next, err := cleanCodes(codes)
	if err != nil {
		return nil, err
	}
	d := diffSets(g.Codes, next)
	res := &SaveResult{Diff: d, Warnings: noViewWarnings(next), Members: g.Group.Members, Total: len(next)}
	if len(d.Added)+len(d.Removed) > 0 {
		res.Warnings = append(res.Warnings, runGroupSaveCheck(id, g.Group.Name, next)...)
	}
	return res, nil
}

// SaveResult 是保存权限组按钮后的结果。
type SaveResult struct {
	Diff
	Warnings []string `json:"warnings"`
	Members  int      `json:"members"`
	Total    int      `json:"total"`
	Changed  bool     `json:"changed"`
}

func joinCodes(list []string) string {
	if len(list) == 0 {
		return "无"
	}
	return strings.Join(list, "、")
}

// SaveGroup 整体替换权限组的按钮授权（dbt_group_perm）；变更前后差异写入审计。
func (s *Service) SaveGroup(a Actor, id uint, codes []string, reason string) (*SaveResult, error) {
	reason = strings.TrimSpace(reason)
	if len([]rune(reason)) > 200 {
		return nil, bad("原因过长（最多 200 个字符）")
	}
	next, err := cleanCodes(codes)
	if err != nil {
		return nil, err
	}
	var res *SaveResult
	var extra []string // 保存前预检（钩子）的额外警告：必须在写库之前算，才能拿“当前状态”和“保存后”比较
	if g0, _ := s.repo.GetGroup(id); g0 != nil {
		if cur0, err := s.repo.GroupButtons(id); err == nil {
			if d0 := diffSets(onlyValid(cur0), next); len(d0.Added)+len(d0.Removed) > 0 {
				extra = runGroupSaveCheck(id, g0.Name, next)
			}
		}
	}
	err = s.repo.InTx(func(tx Repo) error {
		g, err := tx.GetGroup(id)
		if err != nil {
			return err
		}
		if g == nil {
			return notFound("权限组不存在")
		}
		cur, err := tx.GroupButtons(id)
		if err != nil {
			return err
		}
		d := diffSets(onlyValid(cur), next)
		res = &SaveResult{Diff: d, Warnings: noViewWarnings(next), Members: g.Members, Total: len(next), Changed: len(d.Added)+len(d.Removed) > 0}
		if !res.Changed {
			return nil
		}
		if err := tx.ChangeGroupButtons(id, d.Added, d.Removed); err != nil {
			return err
		}
		info := fmt.Sprintf("权限组 %s（#%d，成员 %d 人）：新增 %s；移除 %s", g.Name, g.ID, g.Members, joinCodes(d.Added), joinCodes(d.Removed))
		if reason != "" {
			info += "；原因：" + reason
		}
		return s.audit(tx, a, "修改按钮权限", info)
	})
	if err != nil {
		return nil, err
	}
	s.invalidate()
	if res != nil && res.Changed {
		res.Warnings = append(res.Warnings, extra...)
	}
	return res, nil
}

// ---- 用户：有效权限与单独授权 / 禁用 ----

// EffectiveItem 是一个按钮对用户的有效结果。
type EffectiveItem struct {
	Module   string     `json:"module"`
	Page     string     `json:"page"`
	Code     string     `json:"code"`
	Label    string     `json:"label"`
	Kind     Kind       `json:"kind"`
	Locked   bool       `json:"locked"`
	Allowed  bool       `json:"allowed"`
	Source   string     `json:"source"`
	Groups   []string   `json:"groups,omitempty"`
	ExpireAt *time.Time `json:"expire_at,omitempty"`
}

// OverrideView 是单独授权 / 禁用的一条，附带是否已到期。
type OverrideView struct {
	Override
	Label   string `json:"label"`
	Expired bool   `json:"expired"`
}

// UserView 是用户的按钮级有效权限。
type UserView struct {
	User      UserInfo        `json:"user"`
	Items     []EffectiveItem `json:"items"`
	Overrides []OverrideView  `json:"overrides"`
}

// User 返回用户对每个按钮的有效结果与来源，以及全部单独授权 / 禁用（含已到期的）。
func (s *Service) User(uid uint) (*UserView, error) {
	u, err := s.repo.GetUser(uid)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, notFound("用户不存在")
	}
	r := s.res
	if r == nil {
		r = New(s.repo)
	}
	ds, p, err := r.Explain(uid)
	if err != nil {
		return nil, err
	}
	out := &UserView{User: *u, Items: []EffectiveItem{}, Overrides: []OverrideView{}}
	for _, m := range Tree {
		for _, pg := range m.Pages {
			for _, b := range pg.Buttons {
				d := ds[b.Code]
				if u.Superuser && u.Active {
					d = Decision{Allowed: true, Source: SrcSuper}
				}
				out.Items = append(out.Items, EffectiveItem{Module: m.Name, Page: pg.Name, Code: b.Code, Label: b.Label, Kind: b.Kind, Locked: b.Locked,
					Allowed: d.Allowed, Source: d.Source, Groups: d.Groups, ExpireAt: d.Expire})
			}
		}
	}
	now := s.now()
	for _, o := range p.Overrides {
		lb := o.Code
		if b, ok := Lookup(o.Code); ok {
			lb = b.Label
		}
		out.Overrides = append(out.Overrides, OverrideView{Override: o, Label: lb, Expired: o.Expired(now)})
	}
	return out, nil
}

// OverrideReq 是单独授权 / 禁用请求。
type OverrideReq struct {
	Effect   string     `json:"effect"` // allow / deny
	Codes    []string   `json:"codes"`
	ExpireAt *time.Time `json:"expire_at"` // 空表示长期
	Reason   string     `json:"reason"`
}

// SetOverrides 给用户单独授权或禁用一批按钮（已存在的按钮会被覆盖为新的效果 / 到期 / 原因）。原因必填并写入审计。
func (s *Service) SetOverrides(a Actor, uid uint, in OverrideReq) error {
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return bad("请填写原因（会写入系统审计）")
	}
	if len([]rune(reason)) > 200 {
		return bad("原因过长（最多 200 个字符）")
	}
	if in.Effect != EffectAllow && in.Effect != EffectDeny {
		return bad("方式只能是单独授权或单独禁用")
	}
	codes, err := cleanCodes(in.Codes)
	if err != nil {
		return err
	}
	if len(codes) == 0 {
		return bad("请选择至少一个按钮")
	}
	now := s.now()
	if in.ExpireAt != nil && !in.ExpireAt.After(now) {
		return bad("到期时间必须晚于当前时间")
	}
	err = s.repo.InTx(func(tx Repo) error {
		u, err := tx.GetUser(uid)
		if err != nil {
			return err
		}
		if u == nil {
			return notFound("用户不存在")
		}
		if u.Superuser {
			return bad("超级用户绕过全部权限检查，无需单独授权 / 禁用")
		}
		before, err := tx.UserOverrides(uid)
		if err != nil {
			return err
		}
		prev := map[string]string{}
		for _, o := range before {
			prev[o.Code] = o.Effect
		}
		list := make([]Override, 0, len(codes))
		var chg []string
		for _, c := range codes {
			list = append(list, Override{Code: c, Effect: in.Effect, ExpireAt: in.ExpireAt, Reason: reason, CreatedBy: cut(a.Username, 30), CreatedAt: now})
			old := prev[c]
			if old == "" {
				old = "无"
			}
			chg = append(chg, fmt.Sprintf("%s（%s ⇒ %s）", c, effectLabel(old), effectLabel(in.Effect)))
		}
		if err := tx.UpsertOverrides(uid, list); err != nil {
			return err
		}
		exp := "长期"
		if in.ExpireAt != nil {
			exp = fmt.Sprintf("到期 %s", in.ExpireAt.Format("2006-01-02 15:04"))
		}
		action := "单独授权按钮"
		if in.Effect == EffectDeny {
			action = "单独禁用按钮"
		}
		return s.audit(tx, a, action, fmt.Sprintf("用户 %s（#%d）：%s；%s；原因：%s", u.Username, u.ID, strings.Join(chg, "、"), exp, reason))
	})
	if err != nil {
		return err
	}
	s.invalidate()
	return nil
}

func effectLabel(e string) string {
	switch e {
	case EffectAllow:
		return "单独授权"
	case EffectDeny:
		return "单独禁用"
	}
	return e
}

// RemoveOverrides 撤销用户的一批单独授权 / 禁用。
func (s *Service) RemoveOverrides(a Actor, uid uint, codes []string, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return bad("请填写原因（会写入系统审计）")
	}
	if len([]rune(reason)) > 200 {
		return bad("原因过长（最多 200 个字符）")
	}
	seen := map[string]bool{}
	var list []string
	for _, c := range codes {
		if c = strings.TrimSpace(c); c != "" && !seen[c] {
			seen[c] = true
			list = append(list, c)
		}
	}
	if len(list) == 0 {
		return bad("请选择至少一个按钮")
	}
	err := s.repo.InTx(func(tx Repo) error {
		u, err := tx.GetUser(uid)
		if err != nil {
			return err
		}
		if u == nil {
			return notFound("用户不存在")
		}
		before, err := tx.UserOverrides(uid)
		if err != nil {
			return err
		}
		have := map[string]string{}
		for _, o := range before {
			have[o.Code] = o.Effect
		}
		var del, desc []string
		for _, c := range list {
			if e, ok := have[c]; ok {
				del = append(del, c)
				desc = append(desc, fmt.Sprintf("%s（%s）", c, effectLabel(e)))
			}
		}
		if len(del) == 0 {
			return conflict("所选按钮都没有单独授权 / 禁用")
		}
		if err := tx.DeleteOverrides(uid, del); err != nil {
			return err
		}
		return s.audit(tx, a, "撤销按钮单独授权/禁用", fmt.Sprintf("用户 %s（#%d）：%s；原因：%s", u.Username, u.ID, strings.Join(desc, "、"), reason))
	})
	if err != nil {
		return err
	}
	s.invalidate()
	return nil
}

// ---- 设置 ----

// SettingReq 是修改设置的请求；字段为空表示不改。（迁移状态 mode 只由 dbt-admin perm migrate 写入，这里不能改。）
type SettingReq struct {
	DenyDisplay string `json:"deny_display"`
	Reason      string `json:"reason"`
}

// GetSetting 返回当前设置。
func (s *Service) GetSetting() (Setting, error) {
	set, err := s.repo.Setting()
	return set.Normalize(), err
}

// SetSetting 修改无权限按钮的呈现方式（隐藏 / 置灰）。
func (s *Service) SetSetting(a Actor, in SettingReq) (Setting, error) {
	cur, err := s.repo.Setting()
	if err != nil {
		return cur, err
	}
	cur = cur.Normalize()
	next := cur
	if in.DenyDisplay != "" {
		if in.DenyDisplay != DenyHide && in.DenyDisplay != DenyDisable {
			return cur, bad("无权限按钮的呈现方式只能是 hide 或 disable")
		}
		next.DenyDisplay = in.DenyDisplay
	}
	if next == cur {
		return cur, nil
	}
	reason := strings.TrimSpace(in.Reason)
	err = s.repo.InTx(func(tx Repo) error {
		if err := tx.SaveSetting("deny_display", next.DenyDisplay, cut(a.Username, 30), s.now()); err != nil {
			return err
		}
		info := fmt.Sprintf("无权限按钮：%s ⇒ %s", cur.DenyDisplay, next.DenyDisplay)
		if reason != "" {
			info += "；原因：" + reason
		}
		return s.audit(tx, a, "修改按钮权限设置", info)
	})
	if err != nil {
		return cur, err
	}
	s.invalidate()
	return next, nil
}

// IsAdmin 判断用户是否仍是启用的超级用户（接口每次请求都回库确认）。
func (s *Service) IsAdmin(uid uint) (bool, error) {
	u, err := s.repo.GetUser(uid)
	if err != nil || u == nil {
		return false, err
	}
	return u.Active && u.Superuser, nil
}
