// @author: kevinkelly
// @license: Apache Licence
// @file: perm_test.go
// @time: 2026/10/28

package binlog

import (
	"testing"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/btnperm"
)

// 登记表里的 Binlog 按钮：类型、可授予性、原权限码映射（迁移用），以及按钮判定。
// pageMenu 是页面访问用的原权限码（只给一次性迁移用）。
const pageMenu = "menu_my2sql"

func TestRegistryEntries(t *testing.T) {
	if err := btnperm.Validate(); err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		kind   btnperm.Kind
		super  bool
		legacy []string
	}{
		BtnView:     {btnperm.KindView, false, []string{pageMenu}},
		BtnParse:    {btnperm.KindOp, false, []string{pageMenu}},
		BtnRollback: {btnperm.KindDanger, true, nil},
		BtnDownload: {btnperm.KindDanger, true, nil},
	}
	for code, w := range want {
		b, ok := btnperm.Lookup(code)
		if !ok {
			t.Fatalf("%s 未登记", code)
		}
		if b.Kind != w.kind || b.Super != w.super || b.Locked || !b.Grantable() || len(b.Legacy) != len(w.legacy) {
			t.Errorf("%s: %+v", code, b)
		}
		page, module, _ := btnperm.PageOf(code)
		if page.Key != "binlog" || page.Route != "/binlog" || module != "实例运维" || len(page.Menu) != 1 || page.Menu[0] != pageMenu {
			t.Errorf("%s 所在页面: %+v %s", code, page, module)
		}
	}
	if len(btnperm.ButtonsForLegacy(pageMenu)) == 0 {
		t.Error("menu_my2sql 应有按钮映射（迁移时展开）")
	}
}

func TestButtonEvaluation(t *testing.T) {
	now := time.Now()
	active := &btnperm.Profile{Active: true}
	eval := func(code string, p *btnperm.Profile) bool {
		b, _ := btnperm.Lookup(code)
		return btnperm.EvalButton(b, p, now).Allowed
	}
	// 没有任何授权：四个按钮都不放行（原权限码不参与判定）
	for _, code := range []string{BtnView, BtnParse, BtnRollback, BtnDownload} {
		if eval(code, active) {
			t.Errorf("没有授权不应放行 %s", code)
		}
	}
	// 权限组授予后放行；单独禁用优先
	granted := &btnperm.Profile{Active: true, GroupSource: map[string][]string{BtnView: {"DBA"}, BtnParse: {"DBA"}, BtnRollback: {"DBA"}, BtnDownload: {"DBA"}}}
	for _, code := range []string{BtnView, BtnParse, BtnRollback, BtnDownload} {
		if !eval(code, granted) {
			t.Errorf("权限组授予后 %s 应放行", code)
		}
	}
	denied := &btnperm.Profile{Active: true, GroupSource: granted.GroupSource,
		Overrides: []btnperm.Override{{Code: BtnRollback, Effect: btnperm.EffectDeny}}}
	if eval(BtnRollback, denied) || !eval(BtnDownload, denied) {
		t.Error("单独禁用优先于权限组，且只影响被禁用的按钮")
	}
	// DBA 预设不含需要显式授予的高危按钮
	preset := map[string]bool{}
	for _, c := range btnperm.Preset("DBA") {
		preset[c] = true
	}
	if !preset[BtnView] || !preset[BtnParse] || preset[BtnRollback] || preset[BtnDownload] {
		t.Errorf("DBA 预设: view=%v parse=%v rollback=%v download=%v", preset[BtnView], preset[BtnParse], preset[BtnRollback], preset[BtnDownload])
	}
}

// 审计动作名能被归到“实例管理 / 日志解析”和正确的权限码，回滚与下载标高危。
func TestAuditClassification(t *testing.T) {
	cases := []struct {
		action, code string
		risky        bool
	}{
		{actParse, BtnParse, false}, {actRollback, BtnRollback, true}, {actDownload, BtnDownload, true}, {actCancel, BtnParse, false},
	}
	for _, c := range cases {
		e := audit.Classify(c.action, "实例:mysql-a(#1) 任务:abc")
		if e.Code != c.code || e.Category != audit.CatInst || e.Module != "日志解析" || e.Risky != c.risky || e.Result != audit.ResultOK {
			t.Errorf("Classify(%q) = %+v", c.action, e)
		}
	}
}
