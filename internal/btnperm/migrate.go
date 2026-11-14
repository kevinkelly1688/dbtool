// @author: kevinkelly
// @license: Apache Licence
// @file: migrate.go
// @time: 2026/10/28

package btnperm

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"dbtool/internal/model"
)

// 一次性迁移：把“原权限码”（权限组持有的、单用户直接持有的）展开成按钮授权，并把迁移状态标成 strict。
// 规则：按钮的 Legacy 里有任何一个码被持有，就得到这个按钮（持有某个码 = 得到它对应的全部按钮）；
// 登记表里没有对应按钮的原权限码（早期遗留的、没有判断点的码）丢弃；“登录即可”的基础按钮（BaselineButtons）不写库，也固定开放。
// 迁移只增不减：已有的权限组按钮、单用户授权 / 禁止一律不动；写入的内容在 MigrateResult.Rollback 里给出逐条回滚的 SQL。
// 可重复执行：第二次执行没有任何新增（迁移状态也已经是 strict）。

// MigrateGroupPlan 是一个权限组的迁移计划。
type MigrateGroupPlan struct {
	Group    GroupInfo `json:"group"`
	Legacy   []string  `json:"legacy"`   // 组里现有的原权限码
	Add      []string  `json:"add"`      // 要新增的按钮
	Already  []string  `json:"already"`  // 展开后组里本来就有的按钮（不动）
	Unmapped []string  `json:"unmapped"` // 没有对应按钮、迁移时丢弃的原权限码
}

// MigrateUserPlan 是一个用户的迁移计划：只处理“单独授予”的原权限码。
type MigrateUserPlan struct {
	User     UserInfo `json:"user"`
	Legacy   []string `json:"legacy"`   // 单独授予的原权限码
	Add      []string `json:"add"`      // 要新增的单用户“允许”
	Covered  []string `json:"covered"`  // 迁移后所在权限组已经有，不重复写
	Existing []string `json:"existing"` // 已有单用户覆盖（允许或禁止），不动
	Unmapped []string `json:"unmapped"`
}

// MigratePlan 是整个迁移的计划（只读：PlanMigrate 不写库）。
type MigratePlan struct {
	Groups    []MigrateGroupPlan `json:"groups"`
	Users     []MigrateUserPlan  `json:"users"`
	Baseline  []string           `json:"baseline"`   // 固定对所有登录用户开放的基础按钮（不写库）
	Mode      string             `json:"mode"`       // 现在的迁移状态：strict = 已迁移，compat = 未迁移
	SetStrict bool               `json:"set_strict"` // 迁移时要把迁移状态标成已迁移
}

// Empty 判断计划里是否没有任何要写的东西（第二次执行时为真）。
func (p *MigratePlan) Empty() bool {
	if p.SetStrict {
		return false
	}
	for _, g := range p.Groups {
		if len(g.Add) > 0 {
			return false
		}
	}
	for _, u := range p.Users {
		if len(u.Add) > 0 {
			return false
		}
	}
	return true
}

// Counts 返回要新增的权限组按钮数、单用户授权数。
func (p *MigratePlan) Counts() (groupAdds, userAdds int) {
	for _, g := range p.Groups {
		groupAdds += len(g.Add)
	}
	for _, u := range p.Users {
		userAdds += len(u.Add)
	}
	return
}

// BaselineButtons 返回“登录即可”的基础按钮（登记表里标了 Open：终止自己的工单、查看 / 下载回滚语句、申请查询权限）。
// 它们对所有启用的登录用户固定开放，不进权限树、不写库，迁移时也不用写。
func BaselineButtons() []string {
	var out []string
	for _, c := range idx.codes {
		if b := idx.byCode[c]; b.Open && b.Grantable() {
			out = append(out, c)
		}
	}
	return out
}

// expand 把一组原权限码展开成可授予的按钮码（去重、排序），并返回没有对应按钮的原权限码。
func expand(legacy []string) (buttons, unmapped []string) {
	seen := map[string]bool{}
	for _, x := range legacy {
		bs := ButtonsForLegacy(x)
		if len(bs) == 0 {
			unmapped = append(unmapped, x)
			continue
		}
		for _, b := range bs {
			if b.Grantable() && !seen[b.Code] {
				seen[b.Code] = true
				buttons = append(buttons, b.Code)
			}
		}
	}
	sort.Strings(buttons)
	sort.Strings(unmapped)
	return
}

func minus(list []string, have map[string]bool) (rest, in []string) {
	for _, c := range list {
		if have[c] {
			in = append(in, c)
		} else {
			rest = append(rest, c)
		}
	}
	return
}

// PlanMigrate 读出现有的权限组 / 用户授权，算出迁移计划（只读）。
func PlanMigrate(repo Repo) (*MigratePlan, error) {
	set, err := repo.Setting()
	if err != nil {
		return nil, err
	}
	set = set.Normalize()
	plan := &MigratePlan{Groups: []MigrateGroupPlan{}, Users: []MigrateUserPlan{}, Baseline: BaselineButtons(), Mode: set.Mode, SetStrict: set.Mode != ModeStrict}

	groups, err := repo.ListGroups()
	if err != nil {
		return nil, err
	}
	allBtns, err := repo.AllGroupButtons()
	if err != nil {
		return nil, err
	}
	after := map[string]map[string]bool{} // 权限组名 → 迁移后的按钮集合（用来判断用户的单独授权是否已被所在组覆盖）
	for _, g := range groups {
		legacy, err := repo.GroupDjangoCodes(g.ID)
		if err != nil {
			return nil, err
		}
		want, unmapped := expand(legacy)
		have := map[string]bool{}
		for _, c := range allBtns[g.ID] {
			have[c] = true
		}
		add, already := minus(want, have)
		plan.Groups = append(plan.Groups, MigrateGroupPlan{Group: g, Legacy: legacy, Add: add, Already: already, Unmapped: unmapped})
		next := map[string]bool{}
		for c := range have {
			next[c] = true
		}
		for _, c := range add {
			next[c] = true
		}
		after[g.Name] = next
	}

	direct, err := repo.AllUserDirectCodes()
	if err != nil {
		return nil, err
	}
	groupsOf, err := repo.AllUserGroups()
	if err != nil {
		return nil, err
	}
	uids := make([]uint, 0, len(direct))
	for uid := range direct {
		uids = append(uids, uid)
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	for _, uid := range uids {
		u, err := repo.GetUser(uid)
		if err != nil {
			return nil, err
		}
		if u == nil {
			continue
		}
		legacy := append([]string(nil), direct[uid]...)
		sort.Strings(legacy)
		want, unmapped := expand(legacy)
		covered := map[string]bool{}
		for _, gname := range groupsOf[uid] {
			for c := range after[gname] {
				covered[c] = true
			}
		}
		ovr, err := repo.UserOverrides(uid)
		if err != nil {
			return nil, err
		}
		existing := map[string]bool{}
		for _, o := range ovr {
			existing[o.Code] = true
		}
		rest, cov := minus(want, covered)
		add, ex := minus(rest, existing)
		plan.Users = append(plan.Users, MigrateUserPlan{User: *u, Legacy: legacy, Add: add, Covered: cov, Existing: ex, Unmapped: unmapped})
	}
	return plan, nil
}

// MigrateResult 是执行结果。
type MigrateResult struct {
	GroupAdds int      `json:"group_adds"`
	UserAdds  int      `json:"user_adds"`
	Strict    bool     `json:"strict"`   // 这次把迁移状态标成了 strict（已迁移）
	Rollback  []string `json:"rollback"` // 逐条撤销这次写入的 SQL（只删这次新增的行）
}

// MigrateReason 是写进单用户授权“原因”列的文字。
const MigrateReason = "迁移自原权限码（dbt-admin perm migrate）" // i18n-ignore 存库的授权原因

// RollbackSQL 返回逐条撤销这次迁移写入的 SQL：只删计划里新增的行（已有的权限组按钮、单用户授权 / 禁止不在其中），
// 迁移状态的撤销是删掉 mode 一行（回到“未迁移”）。执行迁移前就可以生成并存档。by 是写进单用户授权 created_by 的操作人。
func (p *MigratePlan) RollbackSQL(by string) []string {
	by = migrateBy(by)
	out := []string{}
	for _, g := range p.Groups {
		if len(g.Add) > 0 {
			out = append(out, fmt.Sprintf("DELETE FROM dbt_group_perm WHERE group_id = %d AND perm_code IN (%s);", g.Group.ID, quoteList(g.Add)))
		}
	}
	for _, u := range p.Users {
		if len(u.Add) > 0 {
			out = append(out, fmt.Sprintf("DELETE FROM dbt_user_perm WHERE user_id = %d AND effect = 'allow' AND created_by = %s AND perm_code IN (%s);", u.User.ID, quote(by), quoteList(u.Add)))
		}
	}
	if p.SetStrict {
		out = append(out, "DELETE FROM dbt_perm_setting WHERE skey = 'mode';")
	}
	return out
}

// migrateBy 规整操作人：空值用 dbt-admin，并截到 created_by 列的 30 个字符。
func migrateBy(by string) string {
	if by == "" {
		by = "dbt-admin"
	}
	return cut(by, 30)
}

// ApplyMigrate 在一个事务里执行计划：新增权限组按钮、新增单用户“允许”、标记为已迁移，并写一条审计。
func ApplyMigrate(repo Repo, plan *MigratePlan, by string, now time.Time) (*MigrateResult, error) {
	by = migrateBy(by)
	res := &MigrateResult{Rollback: plan.RollbackSQL(by)}
	err := repo.InTx(func(tx Repo) error {
		for _, g := range plan.Groups {
			if len(g.Add) == 0 {
				continue
			}
			if err := tx.ChangeGroupButtons(g.Group.ID, g.Add, nil); err != nil {
				return err
			}
			res.GroupAdds += len(g.Add)
		}
		for _, u := range plan.Users {
			if len(u.Add) == 0 {
				continue
			}
			list := make([]Override, len(u.Add))
			for i, c := range u.Add {
				list[i] = Override{Code: c, Effect: EffectAllow, Reason: MigrateReason, CreatedBy: by, CreatedAt: now}
			}
			if err := tx.UpsertOverrides(u.User.ID, list); err != nil {
				return err
			}
			res.UserAdds += len(u.Add)
		}
		if plan.SetStrict {
			if err := tx.SaveSetting("mode", ModeStrict, by, now); err != nil {
				return err
			}
			res.Strict = true
		}
		if res.GroupAdds == 0 && res.UserAdds == 0 && !res.Strict {
			return nil // 没有任何新增：不写审计
		}
		info := fmt.Sprintf("权限组新增按钮 %d 条（%d 个组），单用户新增允许 %d 条（%d 人），标记为已迁移：%v", res.GroupAdds, countGroups(plan), res.UserAdds, countUsers(plan), res.Strict) // i18n-ignore 存库的审计说明
		return tx.InsertAudit(&model.AuditEntry{UserName: cut(by, 30), UserDisplay: cut(by, 50), Action: "迁移原权限到按钮权限（全部）" /* i18n-ignore 存库的审计动作 */, ExtraInfo: info, ActionTime: now})
	})
	if err != nil {
		return nil, err
	}
	if res.GroupAdds == 0 && res.UserAdds == 0 && !res.Strict {
		res.Rollback = []string{}
	}
	return res, nil
}

func countGroups(p *MigratePlan) int {
	n := 0
	for _, g := range p.Groups {
		if len(g.Add) > 0 {
			n++
		}
	}
	return n
}

func countUsers(p *MigratePlan) int {
	n := 0
	for _, u := range p.Users {
		if len(u.Add) > 0 {
			n++
		}
	}
	return n
}

// quote / quoteList 给回滚 SQL 用：按钮码和用户名只含登记表里的字符，这里仍转义单引号和反斜杠。
func quote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `''`).Replace(s) + "'"
}

func quoteList(list []string) string {
	q := make([]string, len(list))
	for i, c := range list {
		q[i] = quote(c)
	}
	return strings.Join(q, ", ")
}
