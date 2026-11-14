// @author: kevinkelly
// @license: Apache Licence
// @file: mapping.go
// @time: 2026/10/28

package authx

import (
	"fmt"
	"sort"
	"strings"
)

// 属性映射：把外部返回的属性（OIDC claims、LDAP 条目属性、钉钉 / 飞书 / 企微接口字段……）对应到平台用户字段。
// 映射值是属性名，可以用 | 写多个备选（取第一个非空的），例如 "preferred_username|email_local"。
// email_local 是内置的派生属性：邮箱 @ 前面的部分。

// defaultAttrSpec 返回各类型的默认映射（LDAP 取 displayname / mail，OIDC 取 preferred_username / name / email）。
func defaultAttrSpec(k Kind) (username, display, email string) {
	switch k {
	case KindLDAP:
		return "", "displayName|cn|name", "mail|email" // 空用户名 = 用登录时输入的账号
	case KindOIDC:
		return "preferred_username|email_local", "name|preferred_username", "email"
	case KindSlack:
		return "email_local", "name|real_name", "email"
	case KindDingTalk:
		return "org_email_local|email_local|userid", "name", "email|org_email"
	case KindFeishu:
		return "user_id|email_local|enterprise_email_local", "name|en_name", "email|enterprise_email"
	case KindWeCom:
		return "userid", "name|userid", "biz_mail|email"
	case KindCAS:
		return "user", "displayName|cn|name|user", "mail|email"
	}
	return "", "", ""
}

// defaultPhoneSpec 返回各类型默认的手机号属性（没有的类型为空）。
func defaultPhoneSpec(k Kind) string {
	switch k {
	case KindLDAP:
		return "mobile|telephoneNumber"
	case KindOIDC:
		return "phone_number"
	case KindDingTalk, KindFeishu, KindWeCom:
		return "mobile"
	case KindCAS:
		return "mobile|phone"
	}
	return ""
}

// lowerAttrs 返回键名统一小写的副本（LDAP 属性名不区分大小写）。
func lowerAttrs(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in)+2)
	for k, v := range in {
		lk := strings.ToLower(k)
		out[lk] = append(out[lk], v...)
	}
	return out
}

// withDerived 增加派生属性（email_local 等），并过滤掉空值。
func withDerived(in map[string][]string) map[string][]string {
	out := lowerAttrs(in)
	for _, src := range []string{"email", "org_email", "enterprise_email", "biz_mail", "mail"} {
		if v := firstValue(out[src]); v != "" {
			if i := strings.LastIndex(v, "@"); i > 0 {
				out[src+"_local"] = []string{v[:i]}
			}
		}
	}
	if v := out["email_local"]; len(v) == 0 {
		for _, src := range []string{"email", "mail", "biz_mail", "org_email"} {
			if l := out[src+"_local"]; len(l) > 0 {
				out["email_local"] = l
				break
			}
		}
	}
	return out
}

func firstValue(vs []string) string {
	for _, v := range vs {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// pickAttr 按映射取值：spec 里 | 分隔的备选依次尝试。
func pickAttr(attrs map[string][]string, spec string) string {
	for _, name := range strings.Split(spec, "|") {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		if v := firstValue(attrs[name]); v != "" {
			return v
		}
	}
	return ""
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

// mapIdentity 把外部属性按该登录方式的映射转成 Identity。
func mapIdentity(p *Provider, subject string, raw map[string][]string, thirdKind, thirdID string, emailVerified bool) *Identity {
	attrs := withDerived(raw)
	du, dd, de := defaultAttrSpec(p.Kind)
	id := &Identity{
		ProviderID: p.ID, Kind: p.Kind, Subject: subject, Attrs: attrs, EmailVerified: emailVerified,
		ThirdKind: thirdKind, ThirdID: thirdID,
	}
	id.Username = pickAttr(attrs, orDefault(p.Config.AttrUsername, du))
	id.Display = pickAttr(attrs, orDefault(p.Config.AttrDisplay, dd))
	id.Email = pickAttr(attrs, orDefault(p.Config.AttrEmail, de))
	if id.Display == "" {
		id.Display = id.Username
	}
	id.Phone = pickAttr(attrs, orDefault(p.Config.AttrPhone, defaultPhoneSpec(p.Kind)))
	return id
}

// evalGroupRules 返回命中的入组规则对应的权限组 / 资源组 ID。
func evalGroupRules(rules []GroupRule, attrs map[string][]string) (auth, res []uint) {
	attrs = lowerAttrs(attrs)
	seenA, seenR := map[uint]bool{}, map[uint]bool{}
	for _, r := range rules {
		if !ruleMatches(r, attrs) {
			continue
		}
		for _, id := range r.AuthGroupIDs {
			if !seenA[id] {
				seenA[id] = true
				auth = append(auth, id)
			}
		}
		for _, id := range r.ResourceGroups {
			if !seenR[id] {
				seenR[id] = true
				res = append(res, id)
			}
		}
	}
	return auth, res
}

func ruleMatches(r GroupRule, attrs map[string][]string) bool {
	vals := attrs[strings.ToLower(strings.TrimSpace(r.Attr))]
	want := strings.ToLower(strings.TrimSpace(r.Value))
	if want == "" {
		return false
	}
	for _, v := range vals {
		v = strings.ToLower(strings.TrimSpace(v))
		switch r.Op {
		case "contains":
			if strings.Contains(v, want) {
				return true
			}
		default: // eq
			if v == want {
				return true
			}
		}
	}
	return false
}

// attrNamesNeeded 返回 LDAP 查询时需要取回的属性：映射到的属性 + 入组规则用到的属性 + memberOf。
func attrNamesNeeded(p *Provider) []string {
	du, dd, de := defaultAttrSpec(p.Kind)
	set := map[string]bool{"memberof": true, "cn": true, "uid": true, "samaccountname": true, "entryuuid": true}
	var add func(spec string)
	add = func(spec string) {
		for _, n := range strings.Split(spec, "|") {
			n = strings.ToLower(strings.TrimSpace(n))
			n = strings.TrimSuffix(n, "_local")
			if n != "" {
				set[n] = true
			}
		}
	}
	add(orDefault(p.Config.AttrUsername, du))
	add(orDefault(p.Config.AttrDisplay, dd))
	add(orDefault(p.Config.AttrEmail, de))
	add(orDefault(p.Config.AttrPhone, defaultPhoneSpec(p.Kind)))
	for _, r := range p.Config.GroupRules {
		set[strings.ToLower(strings.TrimSpace(r.Attr))] = true
	}
	delete(set, "")
	delete(set, "email") // email_local 派生自 mail / email；LDAP 里没有 email 属性名
	set["mail"] = true
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// describeAttrs 把映射结果写成一句话，测试登录时展示（不含密码、令牌）。
func describeIdentity(id *Identity) string {
	return fmt.Sprintf("username=%s　display=%s　email=%s", dash(id.Username), dash(id.Display), dash(id.Email))
}

func dash(s string) string {
	if s == "" {
		return "（空）"
	}
	return s
}
