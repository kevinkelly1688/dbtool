// @author: kevinkelly
// @license: Apache Licence
// @file: users.go
// @time: 2026/10/28

package sysadmin

import (
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"dbtool/internal/audit"
	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// 用户名规则：字母数字和 @.+-_；这里另外限制 30 个字符，因为工单、审计等表里存用户名的列只有 30 位。
var (
	usernameRe = regexp.MustCompile(`^[\p{L}\p{N}_.@+-]+$`)
	notifyIDRe = regexp.MustCompile(`^[A-Za-z0-9_\-.:@]*$`)
)

// UserItem 是用户列表里的一行。
type UserItem struct {
	ID                uint     `json:"id"`
	Username          string   `json:"username"`
	Display           string   `json:"display"`
	Email             string   `json:"email"`
	IsActive          bool     `json:"is_active"`
	IsSuperuser       bool     `json:"is_superuser"`
	AuthGroups        []IDName `json:"auth_groups"`
	ResourceGroups    []IDName `json:"resource_groups"`
	Locked            bool     `json:"locked"`
	LockRemaining     int      `json:"lock_remaining"` // 剩余锁定秒数
	FailedLoginCount  int      `json:"failed_login_count"`
	TwoFA             []string `json:"two_fa"` // 已绑定的 2FA 类型：totp / sms
	LastLogin         string   `json:"last_login"`
	LastLoginFailedAt string   `json:"last_login_failed_at"`
	HasDing           bool     `json:"has_ding"`
	HasFeishu         bool     `json:"has_feishu"`
	HasSlack          bool     `json:"has_slack"`

	// 旁路表 dbt_user_profile 与登录方式绑定（表不存在时为空 / 本地）
	Dept               string   `json:"dept"`
	AuthMethods        []string `json:"auth_methods"` // local / ldap / oidc …
	AuthLabel          string   `json:"auth_label"`   // 中文展示，如 “LDAP”“本地 + 钉钉”
	CanResetPassword   bool     `json:"can_reset"`    // LDAP 用户的密码由外部系统管理，不能在这里重置
	MustChangePassword bool     `json:"must_change"`  // 下次登录必须先修改密码
}

// UserDetail 是编辑用户时用的详情（含通知标识的具体值，不含密码）。
type UserDetail struct {
	UserItem
	DingUserID       string `json:"ding_user_id"`
	SlackID          string `json:"slack_id"`
	FeishuOpenID     string `json:"feishu_open_id"`
	AuthGroupIDs     []uint `json:"auth_group_ids"`
	ResourceGroupIDs []uint `json:"resource_group_ids"`
	LastLoginIP      string `json:"last_login_ip"` // 取自审计日志里最近一次“登入”记录
}

// UserQuery 是用户列表的查询条件。
type UserQuery struct {
	Search          string
	Status          string
	Sort            string // 空 = 用户名；last_login / username / status
	AuthGroupID     uint
	ResourceGroupID uint
	Page            int
	PageSize        int
}

// UserPage 是分页后的用户列表。
type UserPage struct {
	Rows     []UserItem `json:"rows"`
	Total    int64      `json:"total"`
	Page     int        `json:"page"`
	PageSize int        `json:"page_size"`
	Pages    int        `json:"pages"`
}

// UserInput 是新建 / 编辑用户的输入。Password 仅新建时使用，永远不会出现在任何响应里。
type UserInput struct {
	Username         string `json:"username"`
	Display          string `json:"display"`
	Email            string `json:"email"`
	Password         string `json:"password"`
	DingUserID       string `json:"ding_user_id"`
	SlackID          string `json:"slack_id"`
	FeishuOpenID     string `json:"feishu_open_id"`
	IsActive         bool   `json:"is_active"`
	IsSuperuser      bool   `json:"is_superuser"`
	AuthGroupIDs     []uint `json:"auth_group_ids"`
	ResourceGroupIDs []uint `json:"resource_group_ids"`
	// Dept 为 nil 表示不修改（编辑时）；AuthType 仅新建时有效：local（默认）或 ldap。
	Dept     *string `json:"dept"`
	AuthType string  `json:"auth_type"`
}

// lockPolicy 读取登录锁定策略（security.login_lock_attempts 默认 5，security.login_lock_seconds 默认 300 秒）。
func (s *Service) lockPolicy() (int, time.Duration) {
	count, secs := 5, 300
	if v, ok := s.repo.ConfigValue(settings.KeySecurityLoginLockAttempts); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			count = n
		}
	}
	if v, ok := s.repo.ConfigValue(settings.KeySecurityLoginLockSeconds); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			secs = n
		}
	}
	return count, time.Duration(secs) * time.Second
}

func (s *Service) toUserItem(u model.User, ag, rg map[uint][]IDName, tfa map[uint][]string, lockCount int, lockWin time.Duration) UserItem {
	it := UserItem{ID: u.ID, Username: u.Username, Display: u.Display, Email: u.Email, IsActive: u.IsActive, IsSuperuser: u.IsSuperuser,
		AuthGroups: ag[u.ID], ResourceGroups: rg[u.ID], FailedLoginCount: u.FailedLoginCount, TwoFA: tfa[u.ID],
		HasDing: u.DingUserID != "", HasFeishu: u.FeishuOpenID != "", HasSlack: u.SlackID != ""}
	if it.AuthGroups == nil {
		it.AuthGroups = []IDName{}
	}
	if it.ResourceGroups == nil {
		it.ResourceGroups = []IDName{}
	}
	if it.TwoFA == nil {
		it.TwoFA = []string{}
	}
	it.LastLogin = fmtTimePtr(u.LastLogin)
	it.LastLoginFailedAt = fmtTimePtr(u.LastLoginFailedAt)
	if u.FailedLoginCount >= lockCount && u.LastLoginFailedAt != nil {
		if until := u.LastLoginFailedAt.Add(lockWin); until.After(s.now()) {
			it.Locked = true
			it.LockRemaining = int(until.Sub(s.now()).Seconds()) + 1
		}
	}
	return it
}

func (s *Service) userItems(users []model.User) ([]UserItem, error) {
	ids := make([]uint, len(users))
	for i, u := range users {
		ids[i] = u.ID
	}
	ag, err := s.repo.UserAuthGroups(ids)
	if err != nil {
		return nil, err
	}
	rg, err := s.repo.UserResourceGroups(ids)
	if err != nil {
		return nil, err
	}
	tfa, err := s.repo.UserTwoFATypes(ids)
	if err != nil {
		return nil, err
	}
	profiles, err := s.ext().UserProfiles(ids)
	if err != nil {
		return nil, err
	}
	kinds, err := s.ext().UserAuthKinds(ids)
	if err != nil {
		return nil, err
	}
	count, win := s.lockPolicy()
	out := make([]UserItem, 0, len(users))
	for _, u := range users {
		it := s.toUserItem(u, ag, rg, tfa, count, win)
		p := profiles[u.ID]
		it.Dept, it.MustChangePassword = p.Dept, p.MustChangePassword
		it.AuthMethods = authMethods(p.AuthType, kinds[u.ID])
		labels := make([]string, len(it.AuthMethods))
		for i, m := range it.AuthMethods {
			labels[i] = AuthLabel(m)
		}
		it.AuthLabel = strings.Join(labels, " + ")
		it.CanResetPassword = !passwordManagedExternally(it.AuthMethods)
		out = append(out, it)
	}
	return out, nil
}

// ListUsers 返回用户列表（分页、筛选）。
func (s *Service) ListUsers(q UserQuery) (*UserPage, error) {
	switch q.Status {
	case "", StatusActive, StatusLocked, StatusInactive, StatusNo2FA, StatusSuper:
	default:
		return nil, bad("未知的状态筛选")
	}
	switch q.Sort {
	case "", SortUsername, SortLastLogin, SortStatus:
	default:
		return nil, bad("未知的排序方式")
	}
	off, lim, page := Page(q.Page, q.PageSize)
	count, win := s.lockPolicy()
	search := strings.TrimSpace(q.Search)
	var deptIDs []uint
	if search != "" { // 部门在旁路表里：先按部门名找出用户 ID，再与用户名 / 姓名 / 邮箱“或”起来
		ids, err := s.ext().UserIDsByDept(search, 500)
		if err != nil {
			return nil, err
		}
		deptIDs = ids
	}
	users, total, err := s.repo.ListUsers(UserFilter{Search: search, DeptUserIDs: deptIDs, AuthGroupID: q.AuthGroupID, ResourceGroupID: q.ResourceGroupID,
		Status: q.Status, Sort: q.Sort, LockCount: count, LockWindow: win, Now: s.now(), Offset: off, Limit: lim})
	if err != nil {
		return nil, err
	}
	rows, err := s.userItems(users)
	if err != nil {
		return nil, err
	}
	pages := int((total + int64(lim) - 1) / int64(lim))
	if pages < 1 {
		pages = 1
	}
	return &UserPage{Rows: rows, Total: total, Page: page, PageSize: lim, Pages: pages}, nil
}

// UserOptions 是用户页的下拉选项。
type UserOptions struct {
	AuthGroups     []IDName `json:"auth_groups"`
	ResourceGroups []IDName `json:"resource_groups"`
}

func (s *Service) UserOptions() (*UserOptions, error) {
	ag, err := s.repo.ListAuthGroups()
	if err != nil {
		return nil, err
	}
	rg, err := s.repo.ListResourceGroups(ResGroupFilter{})
	if err != nil {
		return nil, err
	}
	out := &UserOptions{AuthGroups: []IDName{}, ResourceGroups: []IDName{}}
	for _, g := range ag {
		out.AuthGroups = append(out.AuthGroups, IDName{g.ID, g.Name})
	}
	for _, g := range rg {
		out.ResourceGroups = append(out.ResourceGroups, IDName{g.GroupID, g.GroupName})
	}
	return out, nil
}

// GetUser 返回用户详情。
func (s *Service) GetUser(id uint) (*UserDetail, error) {
	u, err := s.repo.GetUser(id)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, notFound("用户不存在")
	}
	items, err := s.userItems([]model.User{*u})
	if err != nil {
		return nil, err
	}
	d := &UserDetail{UserItem: items[0], DingUserID: u.DingUserID, SlackID: u.SlackID, FeishuOpenID: u.FeishuOpenID,
		AuthGroupIDs: []uint{}, ResourceGroupIDs: []uint{}}
	for _, g := range d.AuthGroups {
		d.AuthGroupIDs = append(d.AuthGroupIDs, g.ID)
	}
	for _, g := range d.ResourceGroups {
		d.ResourceGroupIDs = append(d.ResourceGroupIDs, g.ID)
	}
	if ip, err := s.ext().UserLastLoginIP(u.Username); err == nil { // 来源 IP 只是参考信息，取不到不影响详情
		d.LastLoginIP = ip
	}
	return d, nil
}

// cleanUserInput 校验并规整用户资料（不含用户名与密码）。
func cleanUserInput(in *UserInput) error {
	in.Display = strings.TrimSpace(in.Display)
	if n := utf8.RuneCountInString(in.Display); n == 0 || n > 10 {
		return bad("姓名需要 1 到 10 个字符")
	}
	in.Email = strings.TrimSpace(in.Email)
	if in.Email != "" {
		if len(in.Email) > 254 {
			return bad("邮箱过长")
		}
		a, err := mail.ParseAddress(in.Email)
		if err != nil || a.Address != in.Email || !strings.Contains(in.Email[strings.IndexByte(in.Email, '@'):], ".") {
			return bad("邮箱格式不正确")
		}
	}
	for _, f := range []struct {
		label string
		v     *string
		max   int
	}{{"钉钉 UserID", &in.DingUserID, 20}, {"Slack ID", &in.SlackID, 50}, {"飞书 OpenID", &in.FeishuOpenID, 50}} {
		*f.v = strings.TrimSpace(*f.v)
		if utf8.RuneCountInString(*f.v) > f.max {
			return bad(errf("%s 最多 %d 个字符", f.label, f.max))
		}
		if !notifyIDRe.MatchString(*f.v) {
			return bad(errf("%s 只能包含字母、数字和 _-.:@", f.label))
		}
	}
	return nil
}

// checkGroupIDs 校验权限组 / 资源组 ID 都存在，返回去重后的 ID 与名称（按 ID 顺序）。
func (s *Service) checkGroupIDs(r Repo, authIDs, resIDs []uint) (auth, res []uint, authNames, resNames []string, err error) {
	if auth, err = dedupIDs(authIDs); err != nil {
		return
	}
	if res, err = dedupIDs(resIDs); err != nil {
		return
	}
	ag, err := r.ListAuthGroups()
	if err != nil {
		return
	}
	an := map[uint]string{}
	for _, g := range ag {
		an[g.ID] = g.Name
	}
	for _, id := range auth {
		n, ok := an[id]
		if !ok {
			err = bad(errf("权限组 #%d 不存在", id))
			return
		}
		authNames = append(authNames, n)
	}
	rg, err := r.ListResourceGroups(ResGroupFilter{})
	if err != nil {
		return
	}
	rn := map[uint]string{}
	for _, g := range rg {
		rn[g.GroupID] = g.GroupName
	}
	for _, id := range res {
		n, ok := rn[id]
		if !ok {
			err = bad(errf("资源组 #%d 不存在或已停用", id))
			return
		}
		resNames = append(resNames, n)
	}
	return
}

// CreateUser 新建用户。密码按 pbkdf2_sha256 格式（pbkdf2_sha256$迭代$盐$哈希）编码，需满足密码策略。
func (s *Service) CreateUser(a audit.Actor, in UserInput) (uint, error) {
	in.Username = strings.TrimSpace(in.Username)
	if n := utf8.RuneCountInString(in.Username); n == 0 || n > 30 || !usernameRe.MatchString(in.Username) {
		return 0, bad("用户名需要 1 到 30 个字符，只能包含字母、数字和 @ . + - _")
	}
	if err := cleanUserInput(&in); err != nil {
		return 0, err
	}
	authType := strings.ToLower(strings.TrimSpace(in.AuthType))
	switch authType {
	case "", authLocal:
		authType = authLocal
	case authLDAP:
	default:
		return 0, bad("认证方式只能是本地或 LDAP")
	}
	dept, err := cleanDept(in.Dept)
	if err != nil {
		return 0, err
	}
	var hash string
	if authType == authLDAP { // LDAP 用户没有本地密码：存一个永远不会匹配的不可用密码
		salt, err := randomSalt()
		if err != nil {
			return 0, err
		}
		hash = "!" + salt
	} else {
		if err := s.policy().Validate(in.Password, in.Username, in.Email, in.Display); err != nil {
			return 0, err
		}
		if hash, err = s.hashPassword(in.Password); err != nil {
			return 0, err
		}
	}
	in.Password = ""
	var id uint
	err = s.tx(a, "新建用户", func(r Repo) (string, error) {
		if ex, err := r.GetUserByName(in.Username); err != nil {
			return "", err
		} else if ex != nil {
			return "", conflict("用户名已存在")
		}
		auth, res, authNames, resNames, err := s.checkGroupIDs(r, in.AuthGroupIDs, in.ResourceGroupIDs)
		if err != nil {
			return "", err
		}
		u := &model.User{Username: in.Username, Password: hash, Display: in.Display, Email: in.Email, DingUserID: in.DingUserID,
			SlackID: in.SlackID, FeishuOpenID: in.FeishuOpenID, IsActive: in.IsActive, IsSuperuser: in.IsSuperuser,
			IsStaff: in.IsSuperuser, DateJoined: s.now()}
		if err := r.CreateUser(u); err != nil {
			return "", err
		}
		if len(auth) > 0 {
			if err := r.SetUserAuthGroups(u.ID, auth); err != nil {
				return "", err
			}
		}
		if len(res) > 0 {
			if err := r.SetUserResourceGroups(u.ID, res); err != nil {
				return "", err
			}
		}
		id = u.ID
		extra := ""
		if dept != "" || authType == authLDAP {
			p := UserProfile{Dept: dept}
			if authType == authLDAP {
				p.AuthType = authLDAP
			}
			if err := extOf(r).UpsertUserProfile(u.ID, p, a.Username, s.now()); err != nil {
				return "", extMissingErr(err, "设置部门 / 选择 LDAP 认证方式")
			}
			extra = errf("；部门 %s；认证方式 %s", orDash(dept), AuthLabel(authType)) // i18n-ignore 存库的审计说明
		}
		return errf("用户 %s（#%d）；超级用户：%s；启用：%s；权限组 %s；资源组 %s%s", u.Username, u.ID, yesNo(u.IsSuperuser), yesNo(u.IsActive), // i18n-ignore 存库的审计说明
			joinNames(authNames), joinNames(resNames), extra), nil
	})
	return id, err
}

// cleanDept 校验并规整部门（nil 视为空）。
func cleanDept(p *string) (string, error) {
	if p == nil {
		return "", nil
	}
	d := strings.TrimSpace(*p)
	if utf8.RuneCountInString(d) > 50 {
		return "", bad("部门最多 50 个字符")
	}
	if strings.ContainsAny(d, "\r\n\t") {
		return "", bad("部门不能包含换行或制表符")
	}
	return d, nil
}

func yesNo(b bool) string {
	if b {
		return "是"
	}
	return "否"
}

// guardSuperuser 检查对 target 的修改是否违反“不能停用/降级自己”“不能留下零个超级用户”。
func (s *Service) guardSuperuser(r Repo, a audit.Actor, target *model.User, newActive, newSuper bool) error {
	if target.ID == a.ID {
		if !newActive {
			return forbid("不能停用自己的账号")
		}
		if target.IsSuperuser && !newSuper {
			return forbid("不能降级自己（取消自己的超级用户身份），请让其他超级用户操作")
		}
	}
	if newSuper != target.IsSuperuser && !a.IsSuperuser {
		return forbid("只有超级用户可以修改超级用户标记")
	}
	if target.IsSuperuser && target.IsActive && (!newActive || !newSuper) {
		n, err := r.CountOtherSuperusers(target.ID)
		if err != nil {
			return err
		}
		if n == 0 {
			return conflict("不能停用或降级最后一个超级用户，请先指定另一个超级用户")
		}
	}
	return nil
}

// UpdateUser 编辑用户资料、状态、超级用户标记、权限组与资源组（用户名与密码不在这里改）。
func (s *Service) UpdateUser(a audit.Actor, id uint, in UserInput) error {
	if err := cleanUserInput(&in); err != nil {
		return err
	}
	newDept, err := cleanDept(in.Dept)
	if err != nil {
		return err
	}
	return s.tx(a, "修改用户", func(r Repo) (string, error) {
		u, err := r.GetUser(id)
		if err != nil {
			return "", err
		}
		if u == nil {
			return "", notFound("用户不存在")
		}
		if strings.TrimSpace(in.Username) != "" && in.Username != u.Username {
			return "", bad("用户名创建后不可修改")
		}
		if err := s.guardSuperuser(r, a, u, in.IsActive, in.IsSuperuser); err != nil {
			return "", err
		}
		auth, res, _, _, err := s.checkGroupIDs(r, in.AuthGroupIDs, in.ResourceGroupIDs)
		if err != nil {
			return "", err
		}
		var changes []string
		fields := map[string]any{}
		text := func(label, column, old, val string) {
			if old != val {
				fields[column] = val
				changes = append(changes, label+"："+old+" → "+val)
			}
		}
		text("姓名", "display", u.Display, in.Display)
		text("邮箱", "email", u.Email, in.Email)
		note := func(label, column, old, val string) { // 通知标识不是机密，但值只在详情里看，审计里只记“已变更”
			if old != val {
				fields[column] = val
				changes = append(changes, label+"：已变更") // i18n-ignore 存库的审计说明
			}
		}
		note("钉钉 UserID", "ding_user_id", u.DingUserID, in.DingUserID)
		note("Slack ID", "slack_id", u.SlackID, in.SlackID)
		note("飞书 OpenID", "feishu_open_id", u.FeishuOpenID, in.FeishuOpenID)
		if u.IsActive != in.IsActive {
			fields["is_active"] = in.IsActive
			changes = append(changes, "启用："+yesNo(u.IsActive)+" → "+yesNo(in.IsActive)) // i18n-ignore 存库的审计说明
		}
		if u.IsSuperuser != in.IsSuperuser {
			fields["is_superuser"] = in.IsSuperuser
			if in.IsSuperuser {
				fields["is_staff"] = true
			}
			changes = append(changes, "超级用户："+yesNo(u.IsSuperuser)+" → "+yesNo(in.IsSuperuser)) // i18n-ignore 存库的审计说明
		}
		if len(fields) > 0 {
			if err := r.UpdateUser(id, fields); err != nil {
				return "", err
			}
		}
		if in.Dept != nil { // 部门在旁路表里
			profs, err := extOf(r).UserProfiles([]uint{id})
			if err != nil {
				return "", err
			}
			cur := profs[id]
			if cur.Dept != newDept {
				old := cur.Dept
				cur.Dept = newDept
				if err := extOf(r).UpsertUserProfile(id, cur, a.Username, s.now()); err != nil {
					return "", extMissingErr(err, "设置部门")
				}
				changes = append(changes, "部门："+orDash(old)+" → "+orDash(newDept)) // i18n-ignore 存库的审计说明
			}
		}
		// 权限组、资源组：与现状比较后再写
		agm, err := r.UserAuthGroups([]uint{id})
		if err != nil {
			return "", err
		}
		rgm, err := r.UserResourceGroups([]uint{id})
		if err != nil {
			return "", err
		}
		agNames, rgNames, err := s.groupNameMaps(r)
		if err != nil {
			return "", err
		}
		if d, err := applySet(auth, agm[id], agNames, func(ids []uint) error { return r.SetUserAuthGroups(id, ids) }, "权限组"); err != nil {
			return "", err
		} else if d != "" {
			changes = append(changes, d)
		}
		if d, err := applySet(res, rgm[id], rgNames, func(ids []uint) error { return r.SetUserResourceGroups(id, ids) }, "资源组"); err != nil {
			return "", err
		} else if d != "" {
			changes = append(changes, d)
		}
		if len(changes) == 0 {
			return errf("目标用户 %s：无变化", u.Username), nil // i18n-ignore 存库的审计说明
		}
		return errf("目标用户 %s：%s", u.Username, strings.Join(changes, "；")), nil // i18n-ignore 存库的审计说明
	})
}

func idNames(list []IDName) []string {
	out := make([]string, len(list))
	for i, g := range list {
		out[i] = g.Name
	}
	return out
}

func sameIDSet(want []uint, have []IDName) bool {
	if len(want) != len(have) {
		return false
	}
	set := map[uint]bool{}
	for _, g := range have {
		set[g.ID] = true
	}
	for _, id := range want {
		if !set[id] {
			return false
		}
	}
	return true
}

// applySet 在权限组 / 资源组集合变化时写入并返回变更描述。names 是 ID → 名称。
func applySet(want []uint, have []IDName, names map[uint]string, set func([]uint) error, label string) (string, error) {
	if sameIDSet(want, have) {
		return "", nil
	}
	if err := set(want); err != nil {
		return "", err
	}
	var after []string
	for _, id := range want {
		after = append(after, names[id])
	}
	return describeDiff(label, idNames(have), after), nil
}

// groupNameMaps 返回权限组、（未停用的）资源组的 ID → 名称。
func (s *Service) groupNameMaps(r Repo) (auth, res map[uint]string, err error) {
	ag, err := r.ListAuthGroups()
	if err != nil {
		return nil, nil, err
	}
	rg, err := r.ListResourceGroups(ResGroupFilter{})
	if err != nil {
		return nil, nil, err
	}
	auth, res = map[uint]string{}, map[uint]string{}
	for _, g := range ag {
		auth[g.ID] = g.Name
	}
	for _, g := range rg {
		res[g.GroupID] = g.GroupName
	}
	return auth, res, nil
}

// SetUserActive 启用 / 停用用户。
func (s *Service) SetUserActive(a audit.Actor, id uint, active bool) error {
	action := "启用用户"
	if !active {
		action = "停用用户"
	}
	return s.tx(a, action, func(r Repo) (string, error) {
		u, err := r.GetUser(id)
		if err != nil {
			return "", err
		}
		if u == nil {
			return "", notFound("用户不存在")
		}
		if u.IsActive == active {
			return "", conflict("用户已经是该状态")
		}
		if err := s.guardSuperuser(r, a, u, active, u.IsSuperuser); err != nil {
			return "", err
		}
		if err := r.UpdateUser(id, map[string]any{"is_active": active}); err != nil {
			return "", err
		}
		return "目标用户 " + u.Username, nil // i18n-ignore 存库的审计说明
	})
}

// UnlockUser 解锁被登录失败次数锁定的账号（失败次数清零）。
func (s *Service) UnlockUser(a audit.Actor, id uint) error {
	return s.tx(a, "解锁账号", func(r Repo) (string, error) {
		u, err := r.GetUser(id)
		if err != nil {
			return "", err
		}
		if u == nil {
			return "", notFound("用户不存在")
		}
		if u.FailedLoginCount == 0 {
			return "", conflict("该账号没有登录失败记录，无需解锁")
		}
		if err := r.UpdateUser(id, map[string]any{"failed_login_count": 0}); err != nil {
			return "", err
		}
		return errf("目标用户 %s（原失败 %d 次）", u.Username, u.FailedLoginCount), nil // i18n-ignore 存库的审计说明
	})
}

// ResetPassword 由管理员设置新密码（需满足密码策略）。密码不会写入日志或审计。
func (s *Service) ResetPassword(a audit.Actor, id uint, newPassword string) error {
	_, err := s.ResetPasswordEx(a, id, ResetPasswordOpts{Password: newPassword})
	return err
}

// ResetTwoFA 删除用户的两步验证配置，让其下次登录时重新绑定。
func (s *Service) ResetTwoFA(a audit.Actor, id uint) error {
	return s.tx(a, "重置 2FA", func(r Repo) (string, error) {
		u, err := r.GetUser(id)
		if err != nil {
			return "", err
		}
		if u == nil {
			return "", notFound("用户不存在")
		}
		n, err := r.DeleteUserTwoFA(id)
		if err != nil {
			return "", err
		}
		if n == 0 {
			return "", conflict("该用户没有绑定两步验证")
		}
		return errf("目标用户 %s，删除 %d 条配置", u.Username, n), nil // i18n-ignore 存库的审计说明
	})
}
