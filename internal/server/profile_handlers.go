// @author: kevinkelly
// @license: Apache Licence
// @file: profile_handlers.go
// @time: 2026/10/28

package server

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"dbtool/internal/btnperm"
	"dbtool/internal/settings"
)

// 个人菜单（顶栏头像）的数据：GET /api/v1/me/profile
//   - 不带参数：菜单要用的概要（认证方式、是否能改密码、两步验证状态），很轻；
//   - ?full=1：个人信息与权限抽屉要用的全部内容（权限组、资源组、可见实例数、按钮权限按页面分组）。
// 只读，只返回当前登录用户自己的信息；取不到的项省略（旁路表 / dbt_2fa_config 等不存在时不报错）。

type myButton struct {
	Code     string   `json:"code"`
	Label    string   `json:"label"`
	Kind     string   `json:"kind"` // v 查看 / o 操作 / d 高危
	Allowed  bool     `json:"allowed"`
	Source   string   `json:"source"` // super / allow / deny / group / legacy / open / none / locked / inactive
	Groups   []string `json:"groups,omitempty"`
	Locked   bool     `json:"locked,omitempty"`
	ExpireAt string   `json:"expire_at,omitempty"`
}

type myPage struct {
	Module  string     `json:"module"`
	Page    string     `json:"page"`
	Buttons []myButton `json:"buttons"`
}

type myButtons struct {
	DenyDisplay string   `json:"deny_display"`
	Total       int      `json:"total"`
	Allowed     int      `json:"allowed"`
	Granted     int      `json:"granted"` // 单独授权（有效）的数量
	Denied      int      `json:"denied"`  // 单独禁用（有效）的数量
	Pages       []myPage `json:"pages"`
}

type myProfile struct {
	Username          string     `json:"username"`
	Display           string     `json:"display"`
	Email             string     `json:"email,omitempty"`
	Dept              string     `json:"dept,omitempty"`
	Superuser         bool       `json:"superuser"`
	AuthMethods       []string   `json:"auth_methods"`
	CanChangePassword bool       `json:"can_change_password"`
	CanBind           bool       `json:"can_bind"`
	TwoFA             string     `json:"two_fa,omitempty"` // enabled / disabled；查不到时省略
	LastLogin         string     `json:"last_login,omitempty"`
	LoginIP           string     `json:"login_ip,omitempty"`
	SessionExpire     string     `json:"session_expire,omitempty"`
	AuthGroups        []string   `json:"auth_groups,omitempty"`
	ResourceGroups    []string   `json:"resource_groups,omitempty"`
	InstanceCount     *int       `json:"instance_count,omitempty"`
	Buttons           *myButtons `json:"buttons,omitempty"`
	ButtonsError      string     `json:"buttons_error,omitempty"`
}

func init() {
	addRoutes(func(s *Server, rt Routes) { rt.API.GET("/me/profile", s.apiMyProfile) })
}

func (s *Server) apiMyProfile(c *gin.Context) {
	p := principalOf(c)
	out := myProfile{Username: p.Username, Display: p.Display, Superuser: p.IsSuperuser, AuthMethods: []string{}, LoginIP: c.ClientIP()}
	if sess := sessionOf(c); sess != nil && sess.Exp > 0 {
		out.SessionExpire = time.Unix(sess.Exp, 0).Format("2006-01-02 15:04")
	}
	passwordUsable := true
	if u, err := s.repo.GetUserByName(p.Username); err == nil && u != nil {
		out.Email = u.Email
		if u.Display != "" {
			out.Display = u.Display
		}
		if u.LastLogin != nil {
			out.LastLogin = u.LastLogin.Format("2006-01-02 15:04:05")
		}
		passwordUsable = u.Password != "" && !strings.HasPrefix(u.Password, "!")
	}
	if db, _, ok := s.DB(); ok {
		s.fillProfileFromDB(db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)}), p.ID, passwordUsable, &out)
		if c.Query("full") != "" {
			s.fillProfileFull(c, db, &out)
		}
	} else if passwordUsable {
		out.AuthMethods = append(out.AuthMethods, "本地")
		out.CanChangePassword = true
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// fillProfileFromDB 补部门、认证方式、两步验证状态、外部账号绑定开关；旁路表 / dbt_2fa_config 取不到就跳过对应项。
func (s *Server) fillProfileFromDB(db *gorm.DB, uid uint, passwordUsable bool, out *myProfile) {
	var prof struct {
		Dept     string
		AuthType string
	}
	hasProfile := db.Raw("SELECT dept, auth_type FROM dbt_user_profile WHERE user_id = ?", uid).Scan(&prof).Error == nil
	out.Dept = prof.Dept

	external := hasProfile && strings.EqualFold(strings.TrimSpace(prof.AuthType), "ldap")
	var idents []struct{ Kind, Name string }
	_ = db.Raw("SELECT p.kind AS kind, p.name AS name FROM dbt_auth_identity i JOIN dbt_auth_provider p ON p.id = i.provider_id WHERE i.user_id = ? ORDER BY i.id", uid).Scan(&idents).Error

	seen := map[string]bool{}
	add := func(label string) {
		if label = strings.TrimSpace(label); label != "" && !seen[label] {
			seen[label] = true
			out.AuthMethods = append(out.AuthMethods, label)
		}
	}
	if passwordUsable && !external {
		add("本地")
	}
	if external {
		add("LDAP")
	}
	for _, it := range idents {
		add(it.Name)
	}
	if len(out.AuthMethods) == 0 {
		add("外部认证")
	}
	out.CanChangePassword = passwordUsable && !external

	var n int64
	if err := db.Raw("SELECT COUNT(*) FROM `dbt_2fa_config` WHERE user_id = ?", uid).Scan(&n).Error; err == nil {
		out.TwoFA = "disabled"
		if n > 0 {
			out.TwoFA = "enabled"
		}
	}
	out.CanBind = settings.Open(db, nil).Bool(settings.KeySecurityAllowSelfBind)
}

// fillProfileFull 补权限组、资源组、可见实例数与按钮权限。
func (s *Server) fillProfileFull(c *gin.Context, db *gorm.DB, out *myProfile) {
	p := principalOf(c)
	quiet := db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})
	var ag, rg []string
	if err := quiet.Raw("SELECT g.name FROM dbt_auth_group g JOIN dbt_sql_users_groups ug ON ug.group_id = g.id WHERE ug.users_id = ? ORDER BY g.name", p.ID).Scan(&ag).Error; err == nil {
		out.AuthGroups = ag
	}
	if err := quiet.Raw("SELECT rg.group_name FROM dbt_resource_group rg JOIN dbt_sql_users_resource_group ur ON ur.resourcegroup_id = rg.group_id WHERE ur.users_id = ? AND rg.is_deleted = 0 ORDER BY rg.group_name", p.ID).Scan(&rg).Error; err == nil {
		out.ResourceGroups = rg
	}
	if n, ok := s.visibleInstanceCount(p.ID, p.IsSuperuser); ok {
		out.InstanceCount = &n
	}

	res := btnperm.Default()
	svc := btnperm.NewService(btnperm.NewGormRepo(db), res)
	v, err := svc.User(p.ID)
	if err != nil {
		slog.Warn("个人信息：计算按钮权限失败", "user", p.Username, "err", err)
		out.ButtonsError = "暂时无法计算按钮权限，请稍后重试"
		return
	}
	b := &myButtons{Pages: []myPage{}}
	if res != nil {
		if set, err := res.Setting(); err == nil {
			b.DenyDisplay = set.DenyDisplay
		}
	}
	for _, o := range v.Overrides {
		if o.Expired {
			continue
		}
		switch o.Effect {
		case btnperm.EffectAllow:
			b.Granted++
		case btnperm.EffectDeny:
			b.Denied++
		}
	}
	for _, it := range v.Items {
		if n := len(b.Pages); n == 0 || b.Pages[n-1].Module != it.Module || b.Pages[n-1].Page != it.Page {
			b.Pages = append(b.Pages, myPage{Module: it.Module, Page: it.Page, Buttons: []myButton{}})
		}
		pg := &b.Pages[len(b.Pages)-1]
		mb := myButton{Code: it.Code, Label: it.Label, Kind: string(it.Kind), Allowed: it.Allowed, Source: it.Source, Groups: it.Groups, Locked: it.Locked}
		if it.ExpireAt != nil {
			mb.ExpireAt = it.ExpireAt.Format("2006-01-02 15:04")
		}
		pg.Buttons = append(pg.Buttons, mb)
		b.Total++
		if it.Allowed {
			b.Allowed++
		}
	}
	out.Buttons = b
}

// visibleInstanceCount 返回用户在各页面实例下拉里能看到的实例数：超级用户和持有“可查询所有实例”的看全部，其余按资源组。
func (s *Server) visibleInstanceCount(uid uint, super bool) (int, bool) {
	if !super {
		if ok, err := s.repo.HasButton(uid, "query.admin_run"); err == nil && ok {
			super = true
		}
	}
	if super {
		list, err := s.repo.ListInstances()
		if err != nil {
			return 0, false
		}
		return len(list), true
	}
	ids, err := s.repo.UserInstanceIDs(uid)
	if err != nil {
		return 0, false
	}
	return len(ids), true
}
