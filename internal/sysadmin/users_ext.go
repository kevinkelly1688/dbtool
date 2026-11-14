// @author: kevinkelly
// @license: Apache Licence
// @file: users_ext.go
// @time: 2026/10/28

package sysadmin

import (
	"crypto/rand"
	"math/big"

	"dbtool/internal/audit"
)

// ResetPasswordOpts 是管理员重置密码的方式：手输新密码，或让系统生成临时密码（仅返回一次）；可要求用户下次登录先改密。
type ResetPasswordOpts struct {
	Password    string `json:"password"`
	Generate    bool   `json:"generate"`
	ForceChange bool   `json:"force_change"` // 生成临时密码时总是强制
}

// ResetPasswordResult 是重置结果。Password 只在系统生成时返回，之后无法再次查看，也不会写入日志或审计。
type ResetPasswordResult struct {
	Password    string `json:"password,omitempty"`
	ForceChange bool   `json:"force_change"`
}

// ResetPasswordEx 由管理员设置新密码。LDAP 等外部认证的用户密码由外部系统管理，拒绝重置。
// 强制改密标记写在旁路表 dbt_user_profile.must_change_password，登录时的强制跳转由登录模块调用 MustChangePassword 实现。
func (s *Service) ResetPasswordEx(a audit.Actor, id uint, opt ResetPasswordOpts) (*ResetPasswordResult, error) {
	u, err := s.repo.GetUser(id)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, notFound("用户不存在")
	}
	if opt.Generate && opt.Password != "" {
		return nil, bad("不能同时手输密码又选择系统生成")
	}
	ext := s.ext()
	profs, err := ext.UserProfiles([]uint{id})
	if err != nil {
		return nil, err
	}
	kinds, err := ext.UserAuthKinds([]uint{id})
	if err != nil {
		return nil, err
	}
	if methods := authMethods(profs[id].AuthType, kinds[id]); passwordManagedExternally(methods) {
		return nil, conflict(errf("该用户使用 %s 认证，密码由外部系统管理，不能在这里重置", AuthLabel(methods[0])))
	}
	pw, force := opt.Password, opt.ForceChange
	policy := s.policy()
	if opt.Generate {
		if pw, err = generateTempPassword(policy, u.Username, u.Email, u.Display); err != nil {
			return nil, err
		}
		force = true
	} else if err := policy.Validate(pw, u.Username, u.Email, u.Display); err != nil {
		return nil, err
	}
	hash, err := s.hashPassword(pw)
	if err != nil {
		return nil, err
	}
	hadFlag := profs[id].MustChangePassword
	err = s.tx(a, "重置密码", func(r Repo) (string, error) {
		if err := r.UpdateUser(id, map[string]any{"password": hash}); err != nil {
			return "", err
		}
		switch {
		case force:
			if err := extOf(r).SetMustChangePassword(id, true, a.Username, s.now()); err != nil {
				return "", extMissingErr(err, "要求用户下次登录修改密码")
			}
		case hadFlag: // 管理员手输了新密码且不要求改密：清掉之前遗留的标记
			if err := extOf(r).SetMustChangePassword(id, false, a.Username, s.now()); err != nil {
				return "", extMissingErr(err, "清除强制改密标记")
			}
		}
		extra := "目标用户 " + u.Username // i18n-ignore 存库的审计说明
		if opt.Generate {
			extra += "；系统生成临时密码（未记录），首次登录强制改密" // i18n-ignore 存库的审计说明
		} else if force {
			extra += "；首次登录强制改密" // i18n-ignore 存库的审计说明
		}
		return extra, nil
	})
	if err != nil {
		return nil, err
	}
	res := &ResetPasswordResult{ForceChange: force}
	if opt.Generate {
		res.Password = pw
	}
	return res, nil
}

const (
	tmpUpper   = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	tmpLower   = "abcdefghijkmnpqrstuvwxyz"
	tmpDigit   = "23456789"
	tmpSpecial = "!@#$%^&*"
)

func randIndex(n int) (int, error) {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, err
	}
	return int(v.Int64()), nil
}

// generateTempPassword 生成满足当前密码策略的随机临时密码：每类字符至少一个，去掉易混字符（0 O 1 l I）。
func generateTempPassword(p PasswordPolicy, related ...string) (string, error) {
	n := p.MinLen + 4
	if n < 14 {
		n = 14
	}
	if n > 32 {
		n = 32
	}
	all := tmpUpper + tmpLower + tmpDigit + tmpSpecial
	pick := func(set string) (byte, error) {
		i, err := randIndex(len(set))
		return set[i], err
	}
	for attempt := 0; attempt < 20; attempt++ {
		buf := make([]byte, 0, n)
		for _, set := range []string{tmpUpper, tmpLower, tmpDigit, tmpSpecial} {
			c, err := pick(set)
			if err != nil {
				return "", err
			}
			buf = append(buf, c)
		}
		for len(buf) < n {
			c, err := pick(all)
			if err != nil {
				return "", err
			}
			buf = append(buf, c)
		}
		for i := len(buf) - 1; i > 0; i-- { // Fisher-Yates 洗牌
			j, err := randIndex(i + 1)
			if err != nil {
				return "", err
			}
			buf[i], buf[j] = buf[j], buf[i]
		}
		pw := string(buf)
		if p.Validate(pw, related...) == nil {
			return pw, nil
		}
	}
	return "", bad("无法生成满足密码策略的临时密码，请改为手输")
}

// UserSummary 是用户页顶部的 KPI。
type UserSummary struct {
	Total    int64 `json:"total"`
	Active   int64 `json:"active"`
	Inactive int64 `json:"inactive"`
	Super    int64 `json:"superuser"`
	Locked   int64 `json:"locked"`
	No2FA    int64 `json:"no_2fa"`
}

// UserSummary 统计用户 KPI（每项一条 COUNT）。
func (s *Service) UserSummary() (*UserSummary, error) {
	count, win := s.lockPolicy()
	base := UserFilter{LockCount: count, LockWindow: win, Now: s.now(), Limit: 1}
	out := &UserSummary{}
	for _, it := range []struct {
		status string
		dst    *int64
	}{{"", &out.Total}, {StatusActive, &out.Active}, {StatusInactive, &out.Inactive}, {StatusSuper, &out.Super}, {StatusLocked, &out.Locked}, {StatusNo2FA, &out.No2FA}} {
		f := base
		f.Status = it.status
		_, n, err := s.repo.ListUsers(f)
		if err != nil {
			return nil, err
		}
		*it.dst = n
	}
	return out, nil
}
